package personaeval

import (
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const schemaSQL = `
CREATE TABLE persona_eval_result (
    id TEXT PRIMARY KEY,
    agent_id TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    layer TEXT NOT NULL CHECK (layer IN ('policy','behavior')),
    passed INTEGER NOT NULL CHECK (passed IN (0,1)),
    score REAL,
    cases_total INTEGER NOT NULL,
    cases_failed INTEGER NOT NULL,
    detail_json TEXT NOT NULL DEFAULT '[]',
    created_at TEXT NOT NULL
);

CREATE INDEX persona_eval_result_lookup_idx ON persona_eval_result(agent_id, fingerprint, layer, created_at);
`

func Schema() sqlite.SchemaOwner {
	return sqlite.StaticSchema{Name: "persona_eval", SQL: schemaSQL}
}
