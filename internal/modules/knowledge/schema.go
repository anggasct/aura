package knowledge

import (
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const schemaSQL = `
CREATE TABLE kb_collection (
    id TEXT PRIMARY KEY,
    generation INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    embedding_fingerprint TEXT
);

CREATE TABLE kb_source (
    rowid INTEGER PRIMARY KEY,
    id TEXT UNIQUE NOT NULL,
    collection TEXT NOT NULL REFERENCES kb_collection(id),
    kind TEXT NOT NULL CHECK (kind IN ('file','url')),
    uri TEXT NOT NULL,
    label TEXT NOT NULL,
    content_sha256 TEXT NOT NULL,
    ingest_version INTEGER NOT NULL,
    ingested_at TEXT NOT NULL,
    UNIQUE (collection, uri)
);

CREATE TABLE kb_chunk (
    rowid INTEGER PRIMARY KEY,
    id TEXT UNIQUE NOT NULL,
    source_id TEXT NOT NULL REFERENCES kb_source(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL,
    heading TEXT,
    content TEXT NOT NULL,
    scope TEXT NOT NULL,
    content_sha256 TEXT NOT NULL,
    UNIQUE (source_id, ordinal)
);

CREATE VIRTUAL TABLE kb_chunk_fts USING fts5(
    content,
    heading,
    scope,
    content='kb_chunk',
    content_rowid='rowid',
    tokenize='porter unicode61 remove_diacritics 2'
);

CREATE TABLE kb_embedding (
    chunk_rowid INTEGER PRIMARY KEY REFERENCES kb_chunk(rowid) ON DELETE CASCADE,
    fingerprint TEXT NOT NULL,
    content_sha256 TEXT NOT NULL,
    vector BLOB NOT NULL
);

CREATE INDEX kb_chunk_source_idx ON kb_chunk(source_id);

CREATE TRIGGER kb_chunk_fts_insert AFTER INSERT ON kb_chunk BEGIN INSERT INTO kb_chunk_fts(rowid, content, heading, scope) VALUES (new.rowid, new.content, new.heading, new.scope); END;

CREATE TRIGGER kb_chunk_fts_delete AFTER DELETE ON kb_chunk BEGIN INSERT INTO kb_chunk_fts(kb_chunk_fts, rowid, content, heading, scope) VALUES ('delete', old.rowid, old.content, old.heading, old.scope); END;

CREATE TRIGGER kb_chunk_fts_update AFTER UPDATE OF content, heading, scope ON kb_chunk BEGIN INSERT INTO kb_chunk_fts(kb_chunk_fts, rowid, content, heading, scope) VALUES ('delete', old.rowid, old.content, old.heading, old.scope); INSERT INTO kb_chunk_fts(rowid, content, heading, scope) VALUES (new.rowid, new.content, new.heading, new.scope); END;

INSERT INTO kb_chunk_fts(kb_chunk_fts, rank) VALUES ('secure-delete', 1);
`

const trustStepVersion = 33

const trustStepSQL = `
ALTER TABLE kb_collection ADD COLUMN trust TEXT NOT NULL DEFAULT 'untrusted' CHECK (trust IN ('untrusted','trusted'));

ALTER TABLE kb_collection ADD COLUMN trusted_at TEXT;

ALTER TABLE kb_source ADD COLUMN vouched_sha256 TEXT;

ALTER TABLE kb_source ADD COLUMN vouched_at TEXT;
`

func Schema() sqlite.SchemaOwner {
	return sqlite.StaticSchema{
		Name:  "knowledge",
		SQL:   schemaSQL,
		Steps: []sqlite.Step{{Version: trustStepVersion, SQL: trustStepSQL}},
	}
}
