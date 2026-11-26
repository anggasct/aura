package model

import (
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const schemaSQL = `
CREATE TABLE model_circuit_checkpoint (
    circuit_key TEXT PRIMARY KEY,
    config_digest TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('closed','open','half_open')),
    consecutive_failures INTEGER NOT NULL CHECK (consecutive_failures >= 0),
    open_until TEXT,
    updated_at TEXT NOT NULL
);
`

func Schema() sqlite.SchemaOwner {
	return sqlite.StaticSchema{Name: "model", SQL: schemaSQL}
}
