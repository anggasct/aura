package turnstore

import (
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const schemaSQL = `
CREATE TABLE session (
    id TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL,
    metadata_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    agent_id TEXT NOT NULL DEFAULT 'main'
);

CREATE TABLE runtime_event (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES session(id) ON DELETE CASCADE,
    sequence INTEGER NOT NULL CHECK (sequence > 0),
    turn_id TEXT NOT NULL,
    invocation_id TEXT NOT NULL,
    branch TEXT NOT NULL DEFAULT '',
    author TEXT NOT NULL,
    kind TEXT NOT NULL,
    schema_version INTEGER NOT NULL CHECK (schema_version > 0),
    payload_json TEXT NOT NULL,
    provider_usage_json TEXT,
    created_at TEXT NOT NULL,
    UNIQUE (session_id, sequence)
);

CREATE TABLE ingress_dedupe (
    source TEXT NOT NULL,
    external_id TEXT NOT NULL,
    accepted_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    turn_id TEXT NOT NULL,
    PRIMARY KEY (source, external_id)
);

CREATE TABLE conversation_session (
    conversation_key TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES session(id) ON DELETE CASCADE,
    updated_at TEXT NOT NULL
);

CREATE INDEX runtime_event_turn_idx ON runtime_event(turn_id, session_id, sequence);

CREATE INDEX conversation_session_session_idx ON conversation_session(session_id);
`

func Schema() sqlite.SchemaOwner {
	return sqlite.StaticSchema{Name: "turn", SQL: schemaSQL}
}
