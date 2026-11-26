package child

import (
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const schemaSQL = `
CREATE TABLE child_run (
    id TEXT PRIMARY KEY,
    idempotency_key TEXT NOT NULL,
    parent_session_id TEXT NOT NULL REFERENCES session(id) ON DELETE CASCADE,
    parent_turn_id TEXT NOT NULL,
    parent_invocation_id TEXT NOT NULL,
    child_session_id TEXT NOT NULL UNIQUE REFERENCES session(id) ON DELETE CASCADE,
    durable_key TEXT NOT NULL,
    context_digest TEXT NOT NULL,
    grants_json TEXT NOT NULL,
    budget_json TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('queued','running','succeeded','failed','cancelled','deadline_exceeded','interrupted')),
    deadline TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    result_status TEXT,
    result_output TEXT,
    result_artifacts_json TEXT NOT NULL DEFAULT '[]',
    tokens_used INTEGER NOT NULL DEFAULT 0,
    cost_micros INTEGER NOT NULL DEFAULT 0,
    completed_at TEXT,
    result_provenance TEXT,
    result_child_id TEXT,
    result_session_id TEXT,
    result_context_digest TEXT,
    result_durable_key TEXT,
    result_source_range TEXT,
    result_model TEXT,
    result_prompt_version TEXT,
    result_trust TEXT,
    UNIQUE(parent_invocation_id, idempotency_key)
);

CREATE INDEX child_parent_state_idx ON child_run(parent_session_id, state, created_at, id);
`

func Schema() sqlite.SchemaOwner {
	return sqlite.StaticSchema{Name: "child", SQL: schemaSQL}
}
