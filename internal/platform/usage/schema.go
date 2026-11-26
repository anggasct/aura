package usage

import (
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const schemaSQL = `
CREATE TABLE usage_reservation (
    id TEXT PRIMARY KEY,
    invocation_id TEXT NOT NULL,
    attempt INTEGER NOT NULL,
    model_definition_id TEXT NOT NULL,
    window_day TEXT NOT NULL,
    window_month TEXT NOT NULL,
    reserved_cost_micros INTEGER NOT NULL CHECK (reserved_cost_micros >= 0),
    state TEXT NOT NULL CHECK (state IN ('active','settled','expired','reconciled')),
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    agent_id TEXT,
    principal_id TEXT,
    principal_class TEXT,
    reserved_tokens INTEGER NOT NULL DEFAULT 0 CHECK (reserved_tokens >= 0),
    UNIQUE(invocation_id, attempt)
);

CREATE TABLE usage_entry (
    id TEXT PRIMARY KEY,
    reservation_id TEXT NOT NULL UNIQUE REFERENCES usage_reservation(id) ON DELETE RESTRICT,
    provider_usage_id TEXT,
    input_tokens INTEGER NOT NULL CHECK (input_tokens >= 0),
    output_tokens INTEGER NOT NULL CHECK (output_tokens >= 0),
    usage_json TEXT NOT NULL,
    cost_micros INTEGER NOT NULL CHECK (cost_micros >= 0),
    accounting TEXT NOT NULL CHECK (accounting IN ('reported','estimated','reconciled')),
    price_version TEXT NOT NULL,
    recorded_at TEXT NOT NULL
);

CREATE INDEX usage_window_idx ON usage_reservation(window_day, window_month, state);

CREATE INDEX usage_reservation_agent_window_idx ON usage_reservation(agent_id, window_month, window_day);

CREATE INDEX usage_reservation_principal_idx ON usage_reservation(principal_id, window_day);
`

func Schema() sqlite.SchemaOwner {
	return sqlite.StaticSchema{Name: "usage", SQL: schemaSQL}
}
