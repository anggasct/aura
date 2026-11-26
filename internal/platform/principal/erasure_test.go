package principal

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/store/storetest"
)

type stubEraser struct {
	fn func(ctx context.Context, state *ErasureState) error
}

func (s stubEraser) ErasePrincipal(ctx context.Context, state *ErasureState) error {
	return s.fn(ctx, state)
}

func countTableRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestEraseRunsEveryParticipantInOneTransaction(t *testing.T) {
	db := storetest.OpenMigrated(t)
	ctx := t.Context()
	const now = "2026-11-02T09:00:00Z"
	for _, stmt := range []string{
		`INSERT INTO principal_grant (principal_id, account, class, granted_at, granted_by) VALUES ('discord:601', 'owner', 'trusted', '` + now + `', 'local-owner')`,
		`INSERT INTO standing_grant (id, principal_id, agent_id, tool_name, granted_at, expires_at, granted_by) VALUES ('sg-1', 'discord:601', 'lobby', 'read_file', '` + now + `', '2026-11-09T09:00:00Z', 'local-owner')`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	boom := errors.New("third participant failed")
	erasers := []Eraser{
		stubEraser{fn: func(ctx context.Context, state *ErasureState) error {
			if _, err := sqlite.Conn(ctx, db).ExecContext(ctx, `DELETE FROM principal_grant WHERE principal_id = ?`, state.Scope().PrincipalID); err != nil {
				return err
			}
			return state.Record(ErasureLineGrants, 1)
		}},
		stubEraser{fn: func(ctx context.Context, state *ErasureState) error {
			if _, err := sqlite.Conn(ctx, db).ExecContext(ctx, `DELETE FROM standing_grant WHERE principal_id = ?`, state.Scope().PrincipalID); err != nil {
				return err
			}
			return state.Record(ErasureLineStandingGrants, 1)
		}},
		stubEraser{fn: func(_ context.Context, _ *ErasureState) error {
			return boom
		}},
	}
	_, err := Erase(ctx, db, ErasureScope{PrincipalID: "discord:601", Apply: true}, erasers)
	if !errors.Is(err, boom) {
		t.Fatalf("Erase = %v, want %v", err, boom)
	}
	if got := countTableRows(t, db, "principal_grant"); got != 1 {
		t.Errorf("principal_grant rows after rollback = %d, want 1", got)
	}
	if got := countTableRows(t, db, "standing_grant"); got != 1 {
		t.Errorf("standing_grant rows after rollback = %d, want 1", got)
	}
}

func TestEraseDryRunWritesNothing(t *testing.T) {
	db := storetest.OpenMigrated(t)
	ctx := t.Context()
	const now = "2026-11-02T09:00:00Z"
	if _, err := db.ExecContext(ctx, `INSERT INTO principal_grant (principal_id, account, class, granted_at, granted_by) VALUES ('discord:601', 'owner', 'trusted', ?, 'local-owner')`, now); err != nil {
		t.Fatalf("seed: %v", err)
	}
	erasers := []Eraser{
		stubEraser{fn: func(ctx context.Context, state *ErasureState) error {
			if _, err := sqlite.Conn(ctx, db).ExecContext(ctx, `DELETE FROM principal_grant WHERE principal_id = ?`, state.Scope().PrincipalID); err != nil {
				return err
			}
			return state.Record(ErasureLineGrants, 1)
		}},
	}
	report, err := Erase(ctx, db, ErasureScope{PrincipalID: "discord:601", Apply: false}, erasers)
	if err != nil {
		t.Fatalf("Erase dry run: %v", err)
	}
	if report.Count(ErasureLineGrants) != 1 {
		t.Fatalf("dry run grants count = %d, want 1", report.Count(ErasureLineGrants))
	}
	if got := countTableRows(t, db, "principal_grant"); got != 1 {
		t.Fatalf("principal_grant rows after dry run = %d, want 1", got)
	}
}

func TestEraseReportPrintsTheDeclaredLineOrder(t *testing.T) {
	db := storetest.OpenMigrated(t)
	erasers := []Eraser{
		stubEraser{fn: func(_ context.Context, state *ErasureState) error {
			if err := state.Record(ErasureLineLiveWorkflowRuns, 12); err != nil {
				return err
			}
			return state.Record(ErasureLineEmailEffects, 8)
		}},
		stubEraser{fn: func(_ context.Context, state *ErasureState) error {
			if err := state.Record(ErasureLineRunSessions, 2); err != nil {
				return err
			}
			return state.Record(ErasureLineSessions, 1)
		}},
	}
	report, err := Erase(t.Context(), db, ErasureScope{PrincipalID: "discord:601", Apply: false}, erasers)
	if err != nil {
		t.Fatalf("Erase: %v", err)
	}
	want := []string{
		"sessions: 1",
		"run_sessions: 2",
		"memory_documents: 0",
		"grants: 0",
		"standing_grants: 0",
		"pairing_requests: 0",
		"usage_links: 0",
		"email_effects: 8",
		"workflow_runs: 0",
		"workflow_approvals: 0",
		"legacy_step_sessions: 0",
		"live_workflow_runs: 12",
	}
	if got := report.Lines(); !slices.Equal(got, want) {
		t.Fatalf("Lines() = %v, want %v", got, want)
	}
}

func TestErasureRefusesAnUndeclaredLine(t *testing.T) {
	t.Run("ErasureState.Record", func(t *testing.T) {
		db := storetest.OpenMigrated(t)
		_, err := Erase(t.Context(), db, ErasureScope{PrincipalID: "discord:601"}, []Eraser{
			stubEraser{fn: func(_ context.Context, state *ErasureState) error {
				return state.Record("unknown_line", 1)
			}},
		})
		if !fault.HasCode(err, ErrorCodeInvalidArgument) {
			t.Fatalf("Record undeclared line = %v, want %s", err, ErrorCodeInvalidArgument)
		}
	})

	t.Run("NewErasureReport", func(t *testing.T) {
		_, err := NewErasureReport(map[string]int{"unknown_line": 1})
		if !fault.HasCode(err, ErrorCodeInvalidArgument) {
			t.Fatalf("NewErasureReport undeclared line = %v, want %s", err, ErrorCodeInvalidArgument)
		}
	})
}
