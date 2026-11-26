package personaeval

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	"github.com/anggasct/aura/internal/platform/turn/store"
)

func TestNewSQLiteStoreRejectsNilDB(t *testing.T) {
	t.Parallel()
	if _, err := NewSQLiteStore(nil); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("NewSQLiteStore(nil) = %v, want %q", err, ErrorCodeInvalidArgument)
	}
}

func TestPersonaEvalResultsAppendAndLatestWins(t *testing.T) {
	t.Parallel()
	db := sqlitetest.OpenMigrated(t, turnstore.Schema(), effect.Schema(), Schema())
	evals, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	ctx := t.Context()
	base := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	if _, found, err := evals.Latest(ctx, "lobby", "digest-a", LayerPolicy); err != nil || found {
		t.Fatalf("Latest before any run = %v, %v", found, err)
	}
	for i, passed := range []bool{true, false} {
		if _, err := evals.Record(ctx, &Result{AgentID: "lobby", Digest: "digest-a", Layer: LayerPolicy, Passed: passed, CasesTotal: 4, CasesFailed: i, FailedCases: []string{"case-" + string(rune('a'+i))}, CreatedAt: base.Add(time.Duration(i) * time.Minute)}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	latest, found, err := evals.Latest(ctx, "lobby", "digest-a", LayerPolicy)
	if err != nil || !found || latest.Passed || latest.CasesFailed != 1 || len(latest.FailedCases) != 1 {
		t.Fatalf("Latest = %+v, %v, %v; want the failing second run", latest, found, err)
	}
	if _, found, _ := evals.Latest(ctx, "lobby", "digest-b", LayerPolicy); found {
		t.Fatal("result leaked to another digest")
	}
	if _, found, _ := evals.Latest(ctx, "lobby", "digest-a", LayerBehavior); found {
		t.Fatal("result leaked to another layer")
	}
	list, err := evals.List(ctx, "lobby", 10)
	if err != nil || len(list) != 2 || list[0].CreatedAt.Before(list[1].CreatedAt) {
		t.Fatalf("List = %+v, %v; want newest first", list, err)
	}
	if _, err := evals.Record(ctx, &Result{AgentID: "lobby", Digest: "d", Layer: "vibes"}); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("unknown layer error = %v, want %q", err, ErrorCodeInvalidArgument)
	}
}

func TestPersonaEvalStoreJoinsCallersTransaction(t *testing.T) {
	t.Parallel()
	db := sqlitetest.OpenMigrated(t, turnstore.Schema(), effect.Schema(), Schema())
	evals, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	ctx := t.Context()
	sentinel := errors.New("rollback")
	err = sqlite.RunInTx(ctx, db, func(txCtx context.Context) error {
		if _, err := evals.Record(txCtx, &Result{
			AgentID:    "lobby",
			Digest:     "digest-tx",
			Layer:      LayerPolicy,
			Passed:     true,
			CasesTotal: 1,
		}); err != nil {
			return err
		}
		latest, found, err := evals.Latest(txCtx, "lobby", "digest-tx", LayerPolicy)
		if err != nil || !found || !latest.Passed {
			t.Fatalf("Latest inside tx = %+v, %v, %v; want passing row", latest, found, err)
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("RunInTx = %v, want sentinel rollback", err)
	}
	if _, found, err := evals.Latest(ctx, "lobby", "digest-tx", LayerPolicy); err != nil || found {
		t.Fatalf("Latest after rollback = %v, %v; want not found", found, err)
	}
}
