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

func Schema() sqlite.SchemaOwner {
	return sqlite.StaticSchema{Name: "schedule", SQL: schemaSQL}
}
