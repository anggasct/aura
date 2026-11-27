package workflow

import (
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const schemaSQL = `
CREATE TABLE workflow_definition (
    id TEXT NOT NULL,
    version INTEGER NOT NULL CHECK (version > 0),
    goal TEXT NOT NULL,
    source TEXT NOT NULL CHECK (source IN ('defined','composed','generated')),
    spec_json TEXT NOT NULL CHECK (json_valid(spec_json)),
    spec_sha256 TEXT NOT NULL,
    created_at TEXT NOT NULL,
    name TEXT,
    PRIMARY KEY (id, version)
);

CREATE TABLE workflow_proposal (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES session(id) ON DELETE CASCADE,
    agent_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    spec_json TEXT NOT NULL CHECK (json_valid(spec_json)),
    spec_sha256 TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('proposed','approved','rejected','expired')),
    decided_at TEXT,
    run_id TEXT,
    created_at TEXT NOT NULL,
    tainted INTEGER NOT NULL DEFAULT 0 CHECK (tainted IN (0,1)),
    plan_text TEXT NOT NULL DEFAULT ''
);

CREATE TABLE workflow_approval (
    definition_id TEXT NOT NULL,
    definition_version INTEGER NOT NULL,
    spec_sha256 TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('installed','proposal','routine')),
    agent_id TEXT,
    approver_id TEXT NOT NULL,
    proposal_id TEXT REFERENCES workflow_proposal(id),
    source_definition_id TEXT,
    source_definition_version INTEGER,
    tainted INTEGER NOT NULL DEFAULT 0 CHECK (tainted IN (0,1)),
    approved_at TEXT NOT NULL,
    PRIMARY KEY (definition_id, definition_version),
    FOREIGN KEY (definition_id, definition_version) REFERENCES workflow_definition(id, version) ON DELETE RESTRICT,
    CHECK ((kind = 'proposal') = (proposal_id IS NOT NULL)),
    CHECK ((kind = 'routine') = (source_definition_id IS NOT NULL AND source_definition_version IS NOT NULL))
);

CREATE TABLE workflow_run (
    id TEXT PRIMARY KEY,
    definition_id TEXT NOT NULL,
    definition_version INTEGER NOT NULL,
    durable_key TEXT UNIQUE,
    goal TEXT NOT NULL,
    input_json TEXT NOT NULL CHECK (json_valid(input_json)),
    status TEXT NOT NULL CHECK ( status IN ('queued','running','suspended','succeeded','failed','cancelled') ),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    principal_id TEXT,
    principal_class TEXT CHECK (principal_class IS NULL OR principal_class IN ('owner','trusted','public')),
    agent_id TEXT,
    origin_session_id TEXT,
    reply_account TEXT,
    reply_channel_id TEXT,
    completion_notified_at TEXT,
    completion_attempts INTEGER NOT NULL DEFAULT 0,
    tainted INTEGER NOT NULL DEFAULT 0 CHECK (tainted IN (0,1)),
    parent_run_id TEXT REFERENCES workflow_run(id),
    FOREIGN KEY (definition_id, definition_version) REFERENCES workflow_definition(id, version) ON DELETE RESTRICT
);

CREATE TABLE workflow_step_run (
    run_id TEXT NOT NULL REFERENCES workflow_run(id) ON DELETE CASCADE,
    step_id TEXT NOT NULL,
    status TEXT NOT NULL CHECK ( status IN ('pending','ready','running','succeeded','failed','skipped') ),
    attempt INTEGER NOT NULL CHECK (attempt >= 0),
    started_at TEXT,
    ended_at TEXT,
    output_json TEXT CHECK ( output_json IS NULL OR (json_valid(output_json) AND length(output_json) <= 65536) ),
    output_artifact_digest TEXT,
    error_code TEXT,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (run_id, step_id)
);

CREATE TABLE workflow_correlation (
    source TEXT NOT NULL,
    event_type TEXT NOT NULL,
    external_id TEXT NOT NULL,
    run_id TEXT NOT NULL REFERENCES workflow_run(id) ON DELETE CASCADE,
    signal_name TEXT NOT NULL,
    dedupe_key TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (source, event_type, external_id, dedupe_key)
);

CREATE INDEX workflow_proposal_status_idx ON workflow_proposal(status, created_at);

CREATE INDEX workflow_run_status_idx ON workflow_run(status, created_at);

CREATE INDEX workflow_run_principal_idx ON workflow_run(principal_id, created_at);

CREATE INDEX workflow_run_completion_idx ON workflow_run(status, updated_at) WHERE origin_session_id IS NOT NULL AND completion_notified_at IS NULL AND parent_run_id IS NULL;

CREATE INDEX workflow_correlation_run_idx ON workflow_correlation(run_id);

CREATE TRIGGER workflow_proposal_plan_text_immutable BEFORE UPDATE OF plan_text ON workflow_proposal WHEN NEW.plan_text IS NOT OLD.plan_text BEGIN SELECT RAISE(ABORT, 'workflow_proposal.plan_text is immutable'); END;
`

const replyTargetStepSQL = `
ALTER TABLE workflow_run ADD COLUMN reply_channel TEXT CHECK (reply_channel IS NULL OR (reply_channel IN ('discord','telegram') AND reply_account IS NOT NULL AND reply_channel_id IS NOT NULL));
ALTER TABLE workflow_run ADD COLUMN reply_thread_id TEXT CHECK (reply_thread_id IS NULL OR reply_channel IS NOT NULL);
UPDATE workflow_run SET reply_account = NULL, reply_channel_id = NULL WHERE reply_account IS NULL OR reply_channel_id IS NULL;
UPDATE workflow_run SET reply_channel = 'discord' WHERE reply_channel_id IS NOT NULL;
CREATE TRIGGER workflow_run_reply_target_insert BEFORE INSERT ON workflow_run WHEN NEW.reply_channel IS NULL AND (NEW.reply_account IS NOT NULL OR NEW.reply_channel_id IS NOT NULL) BEGIN SELECT RAISE(ABORT, 'workflow_run reply target is partial'); END;
CREATE TRIGGER workflow_run_reply_target_update BEFORE UPDATE OF reply_channel, reply_account, reply_channel_id ON workflow_run WHEN NEW.reply_channel IS NULL AND (NEW.reply_account IS NOT NULL OR NEW.reply_channel_id IS NOT NULL) BEGIN SELECT RAISE(ABORT, 'workflow_run reply target is partial'); END;
ALTER TABLE workflow_proposal ADD COLUMN reply_channel TEXT CHECK (reply_channel IS NULL OR reply_channel IN ('discord','telegram'));
ALTER TABLE workflow_proposal ADD COLUMN reply_account TEXT;
ALTER TABLE workflow_proposal ADD COLUMN reply_channel_id TEXT;
ALTER TABLE workflow_proposal ADD COLUMN reply_thread_id TEXT CHECK ((reply_channel IS NULL) = (reply_account IS NULL) AND (reply_channel IS NULL) = (reply_channel_id IS NULL) AND (reply_thread_id IS NULL OR reply_channel IS NOT NULL));
`

const replyTargetStepVersion = 23

const personaPinsStepSQL = `
ALTER TABLE workflow_approval ADD COLUMN persona_pins TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(persona_pins));
CREATE TRIGGER workflow_approval_persona_pins_immutable
BEFORE UPDATE OF persona_pins ON workflow_approval
BEGIN
    SELECT RAISE(ABORT, 'workflow_approval.persona_pins is immutable');
END;
`

const personaPinsStepVersion = 24

const stepLabelsStepSQL = `
ALTER TABLE workflow_step_run ADD COLUMN tainted INTEGER NOT NULL DEFAULT 1 CHECK (tainted IN (0,1));
ALTER TABLE workflow_step_run ADD COLUMN private INTEGER NOT NULL DEFAULT 1 CHECK (private IN (0,1));
`

const stepLabelsStepVersion = 25

func Schema() sqlite.SchemaOwner {
	return sqlite.StaticSchema{
		Name: "workflow",
		SQL:  schemaSQL,
		Steps: []sqlite.Step{
			{Version: replyTargetStepVersion, SQL: replyTargetStepSQL},
			{Version: personaPinsStepVersion, SQL: personaPinsStepSQL},
			{Version: stepLabelsStepVersion, SQL: stepLabelsStepSQL},
		},
	}
}
