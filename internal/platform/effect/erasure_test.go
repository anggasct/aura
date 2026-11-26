package effect

import (
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	"github.com/anggasct/aura/internal/platform/turn/store"
)

func TestSessionApprovalEraserRefusesANilReader(t *testing.T) {
	db := sqlitetest.OpenMigrated(t, turnstore.Schema(), Schema())
	if _, err := NewSessionApprovalEraser(db, nil, false); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("NewSessionApprovalEraser(db, nil, false) = %v, want %s", err, ErrorCodeInvalidArgument)
	}
}

func TestDeleteApprovalsForSessionsRemovesOnlyTargetSessions(t *testing.T) {
	db := sqlitetest.OpenMigrated(t, turnstore.Schema(), Schema())
	ctx := t.Context()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, sess := range []string{"wf-s1", "wf-s2"} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO session (id, owner_id, agent_id, metadata_json, created_at, updated_at) VALUES (?, 'workflow', 'main', '{}', ?, ?)`,
			sess, now, now,
		); err != nil {
			t.Fatalf("seed session %s: %v", sess, err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO effect_intent (id, idempotency_key, session_id, turn_id, tool_call_id, provider, operation, classification, request_digest, request_json, state, prepared_at, updated_at)
			VALUES (?, ?, ?, 't1', 'tc1', 'builtin-tools', 'write_file', 'effectful', 'dig', '{}', 'prepared', ?, ?)`,
			"intent-"+sess, "idem-"+sess, sess, now, now,
		); err != nil {
			t.Fatalf("seed intent %s: %v", sess, err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO effect_approval (id, intent_id, action, request_digest, owner_id, reason, issued_at, expires_at, token_hash)
			VALUES (?, ?, 'retry', 'dig', 'local-owner', 'ok', ?, ?, ?)`,
			"appr-"+sess, "intent-"+sess, now, now, "tok-"+sess,
		); err != nil {
			t.Fatalf("seed approval %s: %v", sess, err)
		}
	}

	if err := DeleteApprovalsForSessions(ctx, nil, []string{"wf-s1"}); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("DeleteApprovalsForSessions(nil) = %v, want %s", err, ErrorCodeInvalidArgument)
	}
	if err := DeleteApprovalsForSessions(ctx, db, nil); err != nil {
		t.Fatalf("DeleteApprovalsForSessions(empty): %v", err)
	}
	if err := DeleteApprovalsForSessions(ctx, db, []string{"wf-s1"}); err != nil {
		t.Fatalf("DeleteApprovalsForSessions: %v", err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM effect_approval WHERE id = 'appr-wf-s1'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("target approval count = %d, %v; want 0", count, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM effect_approval WHERE id = 'appr-wf-s2'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("other approval count = %d, %v; want 1", count, err)
	}
}
