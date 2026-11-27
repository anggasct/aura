package operation_test

import (
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
)

func TestOperationSchemaAddsTheReceiptTable(t *testing.T) {
	db := sqlitetest.OpenMigrated(t, operation.Schema())
	insert := `INSERT INTO operation_receipt (idempotency_key, operation_id, request_digest, surface, result_json, created_at) VALUES (?, ?, ?, ?, ?, ?)`
	now := time.Now().UTC().Format(time.RFC3339Nano)

	cases := []struct {
		name    string
		args    []any
		wantErr bool
	}{
		{name: "an agent receipt", args: []any{"k1", "stub.change", "d", "agent", `{}`, now}},
		{name: "a chat receipt", args: []any{"k2", "stub.change", "d", "chat", `{}`, now}},
		{name: "a CLI receipt", args: []any{"k3", "stub.change", "d", "cli", `{}`, now}, wantErr: true},
		{name: "an empty key", args: []any{"", "stub.change", "d", "agent", `{}`, now}, wantErr: true},
		{name: "a repeated key", args: []any{"k1", "stub.change", "d", "agent", `{}`, now}, wantErr: true},
		{name: "a result that is not JSON", args: []any{"k4", "stub.change", "d", "agent", `{`, now}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.ExecContext(t.Context(), insert, tc.args...)
			if tc.wantErr != (err != nil) {
				t.Fatalf("insert error = %v, want error %t", err, tc.wantErr)
			}
		})
	}
	t.Run("the retention index exists", func(t *testing.T) {
		var name string
		err := db.QueryRowContext(t.Context(), `SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'operation_receipt_created_idx'`).Scan(&name)
		if err != nil {
			t.Fatalf("find the index: %v", err)
		}
	})
}
