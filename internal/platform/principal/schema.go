package principal

import (
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const schemaSQL = `
CREATE TABLE principal_grant (
    principal_id TEXT NOT NULL,
    account TEXT NOT NULL,
    class TEXT NOT NULL CHECK (class IN ('trusted')),
    granted_at TEXT NOT NULL,
    granted_by TEXT NOT NULL,
    revoked_at TEXT,
    PRIMARY KEY (principal_id, account)
);

CREATE TABLE pairing_request (
    code_sha256 TEXT PRIMARY KEY,
    principal_id TEXT NOT NULL,
    account TEXT NOT NULL,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    failed_attempts INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE standing_grant (
    id TEXT PRIMARY KEY,
    principal_id TEXT NOT NULL,
    agent_id TEXT NOT NULL,
    tool_name TEXT NOT NULL,
    granted_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    granted_by TEXT NOT NULL,
    revoked_at TEXT
);

CREATE TABLE api_key (
    id TEXT PRIMARY KEY CHECK (length(id) = 12),
    key_sha256 TEXT NOT NULL UNIQUE CHECK (length(key_sha256) = 64),
    persona_id TEXT NOT NULL,
    class TEXT NOT NULL CHECK (class IN ('owner','trusted','public')),
    label TEXT NOT NULL,
    created_at TEXT NOT NULL,
    revoked_at TEXT
);

CREATE INDEX pairing_request_account_idx ON pairing_request(account, expires_at);

CREATE INDEX standing_grant_lookup_idx ON standing_grant(principal_id, agent_id, tool_name, expires_at);
`

func Schema() sqlite.SchemaOwner {
	return sqlite.StaticSchema{Name: "principal", SQL: schemaSQL}
}
