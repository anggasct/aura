package effect

import (
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const schemaSQL = `
CREATE TABLE effect_intent (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES session(id) ON DELETE CASCADE,
    turn_id TEXT NOT NULL,
    tool_call_id TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    provider TEXT NOT NULL,
    operation TEXT NOT NULL,
    classification TEXT NOT NULL CHECK (classification IN ('read_only','idempotent','effectful','irreversible')),
    state TEXT NOT NULL CHECK (state IN ('prepared','started','succeeded','unknown','failed')),
    request_digest TEXT NOT NULL,
    request_json TEXT NOT NULL,
    provider_receipt_json TEXT,
    safe_error_code TEXT,
    retry_of TEXT REFERENCES effect_intent(id) ON DELETE SET NULL,
    prepared_at TEXT NOT NULL,
    started_at TEXT,
    finished_at TEXT,
    reconciled_at TEXT,
    updated_at TEXT NOT NULL,
    result_json TEXT,
    result_digest TEXT CHECK ((result_json IS NULL) = (result_digest IS NULL) AND (result_json IS NULL OR (state = 'succeeded' AND typeof(result_json) = 'text' AND typeof(result_digest) = 'text' AND json_valid(result_json) AND length(CAST(result_json AS BLOB)) <= 1048576))),
    UNIQUE (provider, operation, idempotency_key)
);

CREATE TABLE effect_approval (
    id TEXT PRIMARY KEY,
    intent_id TEXT NOT NULL REFERENCES effect_intent(id) ON DELETE RESTRICT,
    action TEXT NOT NULL CHECK (action IN ('mark_succeeded','mark_failed','retry')),
    request_digest TEXT NOT NULL,
    owner_id TEXT NOT NULL,
    reason TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE,
    issued_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    consumed_at TEXT
);

CREATE INDEX effect_intent_recovery_idx ON effect_intent(state, updated_at);

CREATE INDEX effect_intent_turn_idx ON effect_intent(session_id, turn_id, prepared_at);

CREATE INDEX effect_approval_intent_idx ON effect_approval(intent_id, issued_at);
`

const leaseStepSQL = `
ALTER TABLE effect_intent ADD COLUMN lease_token TEXT;
ALTER TABLE effect_intent ADD COLUMN lease_expires_at TEXT;
CREATE INDEX effect_intent_lease_idx ON effect_intent(state, lease_expires_at) WHERE state = 'started';
`

const leaseStepVersion = 26

func Schema() sqlite.SchemaOwner {
	return sqlite.StaticSchema{
		Name:  "effect",
		SQL:   schemaSQL,
		Steps: []sqlite.Step{{Version: leaseStepVersion, SQL: leaseStepSQL}},
	}
}
