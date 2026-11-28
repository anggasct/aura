package effect

import (
	"database/sql"
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

type intentRow struct {
	id, state, receipt string
}

func seedEffectSession(t *testing.T, db *sql.DB, sessionID, ownerID string) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `INSERT INTO session (id, owner_id, metadata_json, created_at, updated_at) VALUES (?,?,?,?,?) ON CONFLICT(id) DO NOTHING`,
		sessionID, ownerID, `{}`, "2026-09-12T12:00:00Z", "2026-09-12T12:00:00Z"); err != nil {
		t.Fatalf("seed session: %v", err)
	}
}

func TestEffectIntentBaselineSchemaAndResultConstraints(t *testing.T) {
	ctx := t.Context()
	migrations, err := sqlite.History(turnstore.Schema(), Schema())
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	db := sqlitetest.OpenMigrations(t, migrations)
	applied, latest, err := sqlite.SchemaVersions(ctx, db, migrations)
	if err != nil || applied != latest || latest < leaseStepVersion {
		t.Fatalf("schema = applied %d latest %d %v, want >= %d", applied, latest, err, leaseStepVersion)
	}
	seedEffectSession(t, db, "sess-1", "owner-1")
	intents := []intentRow{
		{id: "intent-succeeded", state: "succeeded", receipt: `{"output_bytes":2}`},
		{id: "intent-started", state: "started"},
		{id: "intent-unknown", state: "unknown"},
		{id: "intent-failed", state: "failed"},
	}
	insert := func(intent intentRow, key string, resultJSON, resultDigest any) error {
		var receipt any
		if intent.receipt != "" {
			receipt = intent.receipt
		}
		_, err := db.ExecContext(ctx, `
INSERT INTO effect_intent (id, session_id, turn_id, tool_call_id, idempotency_key, provider, operation,
    classification, state, request_digest, request_json, provider_receipt_json, prepared_at, updated_at, result_json, result_digest)
VALUES (?, 'sess-1', 'turn-1', 'call-1', ?, 'builtin-tools', 'exec', 'effectful', ?, 'digest', '{}', ?, '2026-11-13T10:00:00Z', '2026-11-13T10:00:00Z', ?, ?)`,
			intent.id, key, intent.state, receipt, resultJSON, resultDigest)
		return err
	}
	for _, intent := range intents {
		if err := insert(intent, "key-"+intent.id, nil, nil); err != nil {
			t.Fatalf("insert %s: %v", intent.id, err)
		}
	}
	for _, want := range intents {
		var got intentRow
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

	if err := insert(intentRow{id: "intent-with-result", state: "succeeded"}, "key-with-result", `{"ok":true}`, "digest-ok"); err != nil {
		t.Fatalf("insert valid result failed: %v", err)
	}
	invalidCases := []struct {
		name         string
		state        string
		resultJSON   any
		resultDigest any
	}{
		{name: "result without digest", state: "succeeded", resultJSON: `{"ok":true}`, resultDigest: nil},
		{name: "digest without result", state: "succeeded", resultJSON: nil, resultDigest: "digest-only"},
		{name: "result on non-succeeded", state: "failed", resultJSON: `{"ok":false}`, resultDigest: "digest-fail"},
		{name: "invalid json", state: "succeeded", resultJSON: `{bad`, resultDigest: "digest-bad"},
		{name: "oversized json", state: "succeeded", resultJSON: `"` + strings.Repeat("a", 1048576) + `"`, resultDigest: "digest-big"},
	}
	for i, tc := range invalidCases {
		if err := insert(intentRow{id: "intent-invalid-" + tc.name, state: tc.state}, "key-invalid-"+string(rune('a'+i)), tc.resultJSON, tc.resultDigest); err == nil {
			t.Fatalf("case %s accepted invalid result_json/result_digest", tc.name)
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
	if err := insert(intentRow{id: "intent-duplicate", state: "started"}, "key-intent-succeeded", nil, nil); err == nil {
		t.Fatal("a second intent under an existing (provider, operation, idempotency_key) was accepted")
	}
}
