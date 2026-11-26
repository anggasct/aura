package handoff

import (
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const schemaSQL = `
CREATE TABLE session_handoff (
    session_id TEXT PRIMARY KEY REFERENCES session(id) ON DELETE CASCADE,
    reason TEXT NOT NULL,
    opened_at TEXT NOT NULL,
    released_at TEXT,
    principal_id TEXT NOT NULL DEFAULT '',
    agent_id TEXT NOT NULL DEFAULT '',
    account TEXT NOT NULL DEFAULT '',
    reply_channel_id TEXT NOT NULL DEFAULT '',
    channel TEXT NOT NULL DEFAULT 'discord' CHECK (channel IN ('discord','telegram','http','email')),
    reply_thread_id TEXT NOT NULL DEFAULT ''
);

CREATE INDEX session_handoff_open_idx ON session_handoff(released_at, opened_at);
`

func Schema() sqlite.SchemaOwner {
	return sqlite.StaticSchema{Name: "handoff", SQL: schemaSQL}
}
