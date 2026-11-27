package operation

import (
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const receiptStepSQL = `
CREATE TABLE operation_receipt (
    idempotency_key TEXT PRIMARY KEY CHECK (length(idempotency_key) BETWEEN 1 AND 256),
    operation_id TEXT NOT NULL,
    request_digest TEXT NOT NULL,
    surface TEXT NOT NULL CHECK (surface IN ('chat','agent')),
    result_json TEXT NOT NULL CHECK (json_valid(result_json)),
    created_at TEXT NOT NULL
);

CREATE INDEX operation_receipt_created_idx ON operation_receipt(created_at);
`

const receiptStepVersion = 27

func Schema() sqlite.SchemaOwner {
	return sqlite.StaticSchema{
		Name:  "operation",
		Steps: []sqlite.Step{{Version: receiptStepVersion, SQL: receiptStepSQL}},
	}
}
