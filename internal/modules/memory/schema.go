package memory

import (
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const schemaSQL = `
CREATE TABLE memory_document (
    id TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL,
    session_id TEXT NOT NULL REFERENCES session(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('event_text','summary')),
    source_from_sequence INTEGER NOT NULL CHECK (source_from_sequence > 0),
    source_to_sequence INTEGER NOT NULL CHECK (source_to_sequence >= source_from_sequence),
    content TEXT NOT NULL,
    trust_label TEXT NOT NULL CHECK (trust_label IN ('owner_input','untrusted_external','derived_untrusted')),
    prompt_version TEXT,
    model_protocol TEXT,
    model_name TEXT,
    created_at TEXT NOT NULL,
    expires_at TEXT,
    agent_id TEXT NOT NULL DEFAULT 'main',
    UNIQUE (session_id, kind, source_from_sequence, source_to_sequence, prompt_version)
);

CREATE VIRTUAL TABLE memory_document_fts USING fts5(
    content,
    content='memory_document',
    content_rowid='rowid',
    tokenize='unicode61'
);

CREATE INDEX memory_document_source_idx ON memory_document(session_id, source_from_sequence, source_to_sequence);

CREATE INDEX memory_document_owner_agent_idx ON memory_document(owner_id, agent_id, created_at);

CREATE TRIGGER memory_document_fts_insert AFTER INSERT ON memory_document BEGIN INSERT INTO memory_document_fts(rowid, content) VALUES (new.rowid, new.content); END;

CREATE TRIGGER memory_document_fts_delete AFTER DELETE ON memory_document BEGIN INSERT INTO memory_document_fts(memory_document_fts, rowid, content) VALUES ('delete', old.rowid, old.content); END;

CREATE TRIGGER memory_document_fts_update AFTER UPDATE OF content ON memory_document BEGIN INSERT INTO memory_document_fts(memory_document_fts, rowid, content) VALUES ('delete', old.rowid, old.content); INSERT INTO memory_document_fts(rowid, content) VALUES (new.rowid, new.content); END;
`

func Schema() sqlite.SchemaOwner {
	return sqlite.StaticSchema{Name: "memory", SQL: schemaSQL}
}
