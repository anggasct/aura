package schedule

import (
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const schemaSQL = `
CREATE TABLE scheduled_job (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    cron_expression TEXT NOT NULL,
    timezone TEXT NOT NULL,
    prompt TEXT NOT NULL,
    origin_channel TEXT NOT NULL,
    origin_destination TEXT NOT NULL,
    overlap_policy TEXT NOT NULL CHECK (overlap_policy IN ('skip','queue_one')),
    catch_up_grace_seconds INTEGER NOT NULL CHECK (catch_up_grace_seconds >= 0),
    state TEXT NOT NULL CHECK (state IN ('active','paused','deleted')),
    version INTEGER NOT NULL CHECK (version > 0),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    workflow_id TEXT,
    workflow_input_json TEXT CHECK (workflow_input_json IS NULL OR json_valid(workflow_input_json))
);

CREATE TABLE scheduled_occurrence (
    id TEXT PRIMARY KEY,
    job_id TEXT NOT NULL REFERENCES scheduled_job(id) ON DELETE CASCADE,
    job_version INTEGER NOT NULL,
    scheduled_for_utc TEXT NOT NULL,
    state TEXT NOT NULL CHECK ( state IN ('fired','completed','failed', 'expired','skipped_overlap','cancelled') ),
    turn_id TEXT UNIQUE,
    result_event_id TEXT REFERENCES runtime_event(id) ON DELETE SET NULL,
    safe_error_code TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (job_id, scheduled_for_utc)
);

CREATE INDEX scheduled_occurrence_job_idx ON scheduled_occurrence(job_id, scheduled_for_utc);
`

const taintStepVersion = 29

const taintStepSQL = `
ALTER TABLE scheduled_job ADD COLUMN context TEXT NOT NULL DEFAULT 'full' CHECK (context IN ('full','web_only') AND (context = 'full' OR workflow_id IS NULL));

ALTER TABLE scheduled_job ADD COLUMN tainted INTEGER NOT NULL DEFAULT 0 CHECK (tainted IN (0,1));

ALTER TABLE scheduled_job ADD COLUMN taint_source TEXT CHECK ((taint_source IS NOT NULL) = (tainted = 1) AND (taint_source IS NULL OR length(taint_source) BETWEEN 1 AND 120));

UPDATE scheduled_job SET tainted = 1, taint_source = '` + legacyTaintLabel + `'
WHERE id IN (
    SELECT json_extract(result_json, '$.id') FROM operation_receipt
    WHERE operation_id = 'cron.add' AND surface = 'agent' AND json_type(result_json, '$.id') = 'text'
    UNION
    SELECT json_extract(part.value, '$.functionResponse.response.id')
    FROM runtime_event AS event, json_each(event.payload_json, '$.content.parts') AS part
    WHERE event.kind = 'adk_event' AND instr(event.payload_json, 'aura_cron_add') > 0
      AND json_extract(part.value, '$.functionResponse.name') = 'aura_cron_add'
      AND json_type(part.value, '$.functionResponse.response.id') = 'text'
      AND json_type(part.value, '$.functionResponse.response.error') IS NULL
);
`

func Schema() sqlite.SchemaOwner {
	return sqlite.StaticSchema{
		Name:  "schedule",
		SQL:   schemaSQL,
		Steps: []sqlite.Step{{Version: taintStepVersion, SQL: taintStepSQL}},
	}
}
