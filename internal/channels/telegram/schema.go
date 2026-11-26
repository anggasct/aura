package telegram

import (
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const schemaSQL = `
CREATE TABLE telegram_cursor (
    source TEXT PRIMARY KEY,
    next_update_id INTEGER NOT NULL CHECK (next_update_id >= 0),
    updated_at TEXT NOT NULL
);
`

func Schema() sqlite.SchemaOwner {
	return sqlite.StaticSchema{Name: "telegram", SQL: schemaSQL}
}
