package skills

import (
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const schemaSQL = `
CREATE TABLE skill_package (
    id TEXT PRIMARY KEY,
    canonical_name TEXT NOT NULL,
    origin_json TEXT NOT NULL,
    content_digest TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('quarantined','active','disabled','rejected','conflict')),
    validation_json TEXT NOT NULL,
    requested_capabilities_json TEXT NOT NULL DEFAULT '[]',
    granted_capabilities_json TEXT NOT NULL DEFAULT '[]',
    reviewed_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (canonical_name, origin_json, content_digest)
);

CREATE INDEX skill_state_name_idx ON skill_package(state, canonical_name, id);
`

func Schema() sqlite.SchemaOwner {
	return sqlite.StaticSchema{Name: "skills", SQL: schemaSQL}
}
