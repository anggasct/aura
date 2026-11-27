package email

import (
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const schemaSQL = `
CREATE TABLE email_cursor (
    source TEXT PRIMARY KEY,
    uid_validity INTEGER NOT NULL CHECK (uid_validity >= 0),
    last_uid INTEGER NOT NULL CHECK (last_uid >= 0),
    updated_at TEXT NOT NULL
);
`

func Schema() sqlite.SchemaOwner {
	return sqlite.StaticSchema{Name: "email", SQL: schemaSQL}
}
