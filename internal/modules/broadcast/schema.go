package broadcast

import (
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const schemaSQL = `
CREATE TABLE broadcast_destination (
    alias TEXT PRIMARY KEY,
    next_eligible_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE broadcast_item (
    id TEXT PRIMARY KEY,
    producer TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    content_digest TEXT NOT NULL,
    priority TEXT NOT NULL CHECK (priority IN ('urgent','warning','info')),
    destination_alias TEXT NOT NULL,
    content_json TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('held','scheduled','started','succeeded','failed','unknown','cancelled')),
    not_before TEXT NOT NULL,
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    effect_id TEXT REFERENCES effect_intent(id) ON DELETE RESTRICT,
    digest_parent_id TEXT REFERENCES broadcast_item(id) ON DELETE RESTRICT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(producer, idempotency_key)
);

CREATE INDEX broadcast_due_idx ON broadcast_item(state, not_before, priority, created_at, id);
`

const taintStepVersion = 30

const taintStepSQL = `
ALTER TABLE broadcast_item ADD COLUMN tainted INTEGER NOT NULL DEFAULT 0 CHECK (tainted IN (0,1));
`

func Schema() sqlite.SchemaOwner {
	return sqlite.StaticSchema{
		Name:  "broadcast",
		SQL:   schemaSQL,
		Steps: []sqlite.Step{{Version: taintStepVersion, SQL: taintStepSQL}},
	}
}
