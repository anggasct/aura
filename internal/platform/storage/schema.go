package storage

import (
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const schemaSQL = `
CREATE TABLE blob (
    digest TEXT PRIMARY KEY,
    size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
    media_type TEXT NOT NULL,
    relative_path TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
);

CREATE TABLE artifact_ref (
    id TEXT PRIMARY KEY,
    blob_digest TEXT NOT NULL REFERENCES blob(digest) ON DELETE RESTRICT,
    session_id TEXT NOT NULL REFERENCES session(id) ON DELETE CASCADE,
    event_id TEXT REFERENCES runtime_event(id) ON DELETE SET NULL,
    filename TEXT NOT NULL,
    metadata_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL
);

CREATE INDEX artifact_ref_session_idx ON artifact_ref(session_id, created_at, id);
`

func Schema() sqlite.SchemaOwner {
	return sqlite.StaticSchema{Name: "storage", SQL: schemaSQL}
}
