package discord

import (
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const schemaSQL = `
CREATE TABLE channel_resume (
    source TEXT NOT NULL,
    instance TEXT NOT NULL,
    gateway_session_id TEXT NOT NULL,
    last_sequence INTEGER NOT NULL CHECK (last_sequence >= 0),
    config_digest TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (source, instance)
);
`

func Schema() sqlite.SchemaOwner {
	return sqlite.StaticSchema{Name: "channel_resume", SQL: schemaSQL}
}
