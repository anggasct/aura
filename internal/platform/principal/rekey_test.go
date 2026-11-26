package principal

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/store/storetest"
)

type stubRekeyer struct {
	fn func(ctx context.Context, state *RekeyState, move OwnerMove) error
}

func (s stubRekeyer) RekeyOwner(ctx context.Context, state *RekeyState, move OwnerMove) error {
	return s.fn(ctx, state, move)
}

func TestRekeyRunsEveryParticipantInOneTransaction(t *testing.T) {
	db := storetest.OpenMigrated(t)
	ctx := t.Context()
	const at = "2026-11-02T09:00:00Z"
	for _, stmt := range []string{
		`INSERT INTO session (id, owner_id, metadata_json, created_at, updated_at) VALUES ('s-1', 'kuma', '{}', '` + at + `', '` + at + `')`,
		`INSERT INTO session (id, owner_id, metadata_json, created_at, updated_at) VALUES ('s-2', '111', '{}', '` + at + `', '` + at + `')`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed session: %v", err)
		}
	}
	boom := errors.New("second rekeyer failed on second move")
	calls := 0
	rekeyers := []Rekeyer{
		stubRekeyer{fn: func(ctx context.Context, state *RekeyState, move OwnerMove) error {
			res, err := sqlite.Conn(ctx, db).ExecContext(ctx, `UPDATE session SET owner_id = ? WHERE owner_id = ?`, move.To, move.From)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			return state.Record(RekeyLineSessions, n)
		}},
		stubRekeyer{fn: func(_ context.Context, _ *RekeyState, _ OwnerMove) error {
			calls++
			if calls == 2 {
				return boom
			}
			return nil
		}},
	}
	moves := []OwnerMove{
		{From: "kuma", To: OwnerID},
		{From: "111", To: "discord:111"},
	}
	_, err := Rekey(ctx, db, moves, true, rekeyers)
	if !errors.Is(err, boom) {
		t.Fatalf("Rekey = %v, want %v", err, boom)
	}
	var untouched int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session WHERE owner_id IN ('kuma', '111')`).Scan(&untouched); err != nil || untouched != 2 {
		t.Fatalf("untouched sessions after rollback = %d, %v; want 2", untouched, err)
	}
}

func TestRekeyRefusesAnUndeclaredLine(t *testing.T) {
	db := storetest.OpenMigrated(t)
	_, err := Rekey(t.Context(), db, []OwnerMove{{From: "kuma", To: OwnerID}}, false, []Rekeyer{
		stubRekeyer{fn: func(_ context.Context, state *RekeyState, _ OwnerMove) error {
			return state.Record("unknown_line", 1)
		}},
	})
	if !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("Rekey undeclared line = %v, want %s", err, ErrorCodeInvalidArgument)
	}
}

func TestRekeyValidationErrorsAreNotWrapped(t *testing.T) {
	db := storetest.OpenMigrated(t)
	_, err := Rekey(t.Context(), db, []OwnerMove{{From: "", To: OwnerID}}, false, nil)
	if !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("Rekey invalid move = %v, want %s", err, ErrorCodeInvalidArgument)
	}
	if strings.Contains(err.Error(), "rekey owners:") {
		t.Fatalf("validation error = %q, must not be wrapped with rekey owners:", err.Error())
	}
}
