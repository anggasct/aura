package webhook

import (
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const schemaSQL = `
CREATE TABLE webhook_execution (
    id TEXT PRIMARY KEY,
    key_id TEXT NOT NULL,
    event_id TEXT NOT NULL,
    nonce TEXT NOT NULL,
    body_digest TEXT NOT NULL,
    turn_id TEXT NOT NULL UNIQUE,
    state TEXT NOT NULL CHECK (state IN ('accepted','running','completed','failed','cancelled')),
    result_event_id TEXT REFERENCES runtime_event(id) ON DELETE SET NULL,
    error_code TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    session_id TEXT NOT NULL DEFAULT '',
    UNIQUE (key_id, nonce),
    UNIQUE (key_id, event_id)
);

CREATE INDEX webhook_execution_expiry_idx ON webhook_execution(expires_at);

CREATE INDEX webhook_execution_unfinished_idx ON webhook_execution(id) WHERE state IN ('accepted','running');
`

func Schema() sqlite.SchemaOwner {
	return sqlite.StaticSchema{Name: "webhook", SQL: schemaSQL}
}
