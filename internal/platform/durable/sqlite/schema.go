package sqlite

import (
	dbsqlite "github.com/anggasct/aura/internal/platform/sqlite"
)

const schemaSQL = `
CREATE TABLE durable_host (
    owner TEXT PRIMARY KEY,
    expires_at INTEGER NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE durable_run (
    key TEXT PRIMARY KEY,
    handler TEXT NOT NULL,
    payload BLOB NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('running','succeeded','failed','cancelled')),
    detail TEXT NOT NULL DEFAULT '',
    cancel_requested INTEGER NOT NULL DEFAULT 0 CHECK (cancel_requested IN (0, 1)),
    owner TEXT,
    claim INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE durable_action (
    id INTEGER PRIMARY KEY,
    run_key TEXT NOT NULL REFERENCES durable_run(key) ON DELETE CASCADE,
    action_key TEXT NOT NULL,
    outcome TEXT NOT NULL CHECK (outcome IN ('result','failed')),
    result BLOB NOT NULL,
    detail TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    UNIQUE (run_key, action_key)
);

CREATE TABLE durable_signal (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_key TEXT NOT NULL REFERENCES durable_run(key) ON DELETE CASCADE,
    name TEXT NOT NULL,
    seq INTEGER NOT NULL,
    payload BLOB NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE (run_key, name, seq)
);

CREATE TABLE durable_wait (
    run_key TEXT NOT NULL REFERENCES durable_run(key) ON DELETE CASCADE,
    name TEXT NOT NULL,
    seq INTEGER NOT NULL,
    deadline TEXT NOT NULL,
    outcome TEXT NOT NULL CHECK (outcome IN ('pending','signal','timeout')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (run_key, name, seq)
) WITHOUT ROWID;

CREATE TABLE durable_timer (
    run_key TEXT NOT NULL REFERENCES durable_run(key) ON DELETE CASCADE,
    seq INTEGER NOT NULL,
    fire_at TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (run_key, seq)
) WITHOUT ROWID;

CREATE TABLE durable_approval (
    approval_id TEXT PRIMARY KEY,
    run_key TEXT NOT NULL REFERENCES durable_run(key) ON DELETE CASCADE,
    consumed_at TEXT NOT NULL
);

CREATE TABLE durable_session_turn (
    turn_id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES session(id) ON DELETE CASCADE,
    sequence INTEGER NOT NULL CHECK (sequence > 0),
    state TEXT NOT NULL CHECK (state IN ('queued','active')),
    descriptor_json TEXT NOT NULL,
    deadline TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (session_id, sequence),
    FOREIGN KEY (session_id, sequence) REFERENCES runtime_event(session_id, sequence) ON DELETE CASCADE
);

CREATE INDEX durable_run_running_idx ON durable_run(handler) WHERE state = 'running';

CREATE UNIQUE INDEX durable_session_turn_active_idx ON durable_session_turn(session_id) WHERE state = 'active';
`

func Schema() dbsqlite.SchemaOwner {
	return dbsqlite.StaticSchema{Name: "durable", SQL: schemaSQL}
}
