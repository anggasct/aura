package store

import (
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
)

type legacyIntent struct {
	id, state, receipt string
}

func TestMigrationKeepsEffectIntentsWithoutARecordedResult(t *testing.T) {
	ctx := t.Context()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, bootstrapSchemaMigrationTableSQL); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	for _, m := range migrations[:44] {
		if err := applyMigration(ctx, db, m); err != nil {
			t.Fatalf("apply v%d: %v", m.version, err)
		}
	}
	seedMemorySession(t, db, "sess-1", "owner-1")
	legacy := []legacyIntent{
		{id: "intent-succeeded", state: "succeeded", receipt: `{"output_bytes":2}`},
		{id: "intent-started", state: "started"},
		{id: "intent-unknown", state: "unknown"},
		{id: "intent-failed", state: "failed"},
	}
	insert := func(intent legacyIntent, key string) error {
		var receipt any
		if intent.receipt != "" {
			receipt = intent.receipt
		}
		_, err := db.ExecContext(ctx, `
INSERT INTO effect_intent (id, session_id, turn_id, tool_call_id, idempotency_key, provider, operation,
    classification, state, request_digest, request_json, provider_receipt_json, prepared_at, updated_at)
VALUES (?, 'sess-1', 'turn-1', 'call-1', ?, 'builtin-tools', 'exec', 'effectful', ?, 'digest', '{}', ?, '2026-11-13T10:00:00Z', '2026-11-13T10:00:00Z')`,
			intent.id, key, intent.state, receipt)
		return err
	}
	for _, intent := range legacy {
		if err := insert(intent, "key-"+intent.id); err != nil {
			t.Fatalf("insert legacy %s: %v", intent.id, err)
		}
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	applied, latest, err := SchemaVersions(ctx, db)
	if err != nil || applied != 48 || latest != 48 {
		t.Fatalf("schema = applied %d latest %d %v, want 48", applied, latest, err)
	}
	for _, want := range legacy {
		var got legacyIntent
		var receipt, resultJSON, resultDigest sql.NullString
		var requestJSON, requestDigest, operation string
		if err := db.QueryRowContext(ctx, `
SELECT id, state, provider_receipt_json, request_digest, request_json, operation, result_json, result_digest
FROM effect_intent WHERE id = ?`, want.id).Scan(&got.id, &got.state, &receipt, &requestDigest, &requestJSON, &operation, &resultJSON, &resultDigest); err != nil {
			t.Fatalf("read %s: %v", want.id, err)
		}
		if got.state != want.state || receipt.String != want.receipt || receipt.Valid != (want.receipt != "") {
			t.Fatalf("%s = state %q receipt %q, want state %q receipt %q", want.id, got.state, receipt.String, want.state, want.receipt)
		}
		if requestDigest != "digest" || requestJSON != "{}" || operation != "exec" {
			t.Fatalf("%s lost a column: digest %q request %q operation %q", want.id, requestDigest, requestJSON, operation)
		}
		if resultJSON.Valid || resultDigest.Valid {
			t.Fatalf("%s result_json valid = %v, result_digest valid = %v, want both NULL", want.id, resultJSON.Valid, resultDigest.Valid)
		}
	}
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_index_list('effect_intent')`)
	if err != nil {
		t.Fatalf("index list: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var indexes []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan index: %v", err)
		}
		indexes = append(indexes, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("index rows: %v", err)
	}
	for _, want := range []string{"effect_intent_recovery_idx", "effect_intent_turn_idx"} {
		if !slices.Contains(indexes, want) {
			t.Fatalf("indexes = %v, want %s", indexes, want)
		}
	}
	var uniqueIndexes int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_index_list('effect_intent') WHERE "unique" = 1 AND origin = 'u'`).Scan(&uniqueIndexes); err != nil || uniqueIndexes != 1 {
		t.Fatalf("unique constraint indexes = %d, %v, want 1", uniqueIndexes, err)
	}
	if err := insert(legacyIntent{id: "intent-duplicate", state: "started"}, "key-intent-succeeded"); err == nil {
		t.Fatal("a second intent under an existing (provider, operation, idempotency_key) was accepted")
	}
}
