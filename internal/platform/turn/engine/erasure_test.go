package runtimeengine

import (
	"context"
	"database/sql"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/platform/turn/store"
	"github.com/anggasct/aura/internal/store/storetest"
)

func durableRowsOf(t *testing.T, db *sql.DB, turnID string) (runs, actions int) {
	t.Helper()
	if err := db.QueryRowContext(t.Context(), `SELECT (SELECT COUNT(*) FROM durable_run WHERE key = ?1), (SELECT COUNT(*) FROM durable_action WHERE run_key = ?1)`, turnID).Scan(&runs, &actions); err != nil {
		t.Fatalf("count durable rows of %s: %v", turnID, err)
	}
	return runs, actions
}

func TestErasedPrincipalLeavesNoTurnRun(t *testing.T) {
	rig := newDurableHostRig(t, Deps{Config: testLimits(4, 16)}, newHoldingExecutor())
	rig.startHost()
	turns := []struct{ principal, session, turnID string }{
		{principal: "discord:601", session: "lobby/discord:owner/dm:601", turnID: "turn-erased"},
		{principal: "discord:602", session: "lobby/discord:owner/dm:602", turnID: "turn-kept"},
	}
	for _, tc := range turns {
		sess := turn.Session{ID: tc.session, OwnerID: tc.principal}
		if err := turnstore.NewSessionService(rig.db).Create(t.Context(), &sess); err != nil {
			t.Fatalf("create session %s: %v", tc.session, err)
		}
		req := sampleRequest(tc.session, tc.turnID)
		req.PrincipalID = tc.principal
		awaitTerminal(t, callTurn(context.WithoutCancel(t.Context()), rig.engine, req), turn.EventKindTurnCompleted)
	}
	for _, tc := range turns {
		waitRunEnded(t, rig.runtime, tc.turnID)
		if runs, actions := durableRowsOf(t, rig.db, tc.turnID); runs != 1 || actions == 0 {
			t.Fatalf("durable rows of %s before the erasure = %d runs, %d actions; want its run and its journal", tc.turnID, runs, actions)
		}
	}
	orphanedTurns, err := NewOrphanedTurnRunEraser(rig.db, rig.runtime, turnstore.NewTurnStore(rig.db))
	if err != nil {
		t.Fatalf("NewOrphanedTurnRunEraser: %v", err)
	}
	report, err := principal.Erase(t.Context(), rig.db, principal.ErasureScope{PrincipalID: "discord:601", Apply: true}, []principal.Eraser{
		turnstore.NewSessionEraser(rig.db, false),
		orphanedTurns,
	})
	if err != nil || report.Count(principal.ErasureLineSessions) != 1 {
		t.Fatalf("Erase = %v, %v; want one session erased", report.Lines(), err)
	}
	if runs, actions := durableRowsOf(t, rig.db, "turn-erased"); runs != 0 || actions != 0 {
		t.Errorf("durable rows of the erased principal's turn = %d runs, %d actions; want none", runs, actions)
	}
	if runs, actions := durableRowsOf(t, rig.db, "turn-kept"); runs != 1 || actions == 0 {
		t.Errorf("durable rows of the other principal's turn = %d runs, %d actions; want its run and its journal", runs, actions)
	}
}

func TestOrphanedTurnRunEraserRefusesNilPorts(t *testing.T) {
	db := storetest.OpenMigrated(t)
	turns := turnstore.NewTurnStore(db)
	runs := newDurableHostRig(t, Deps{Config: testLimits(4, 16)}, newHoldingExecutor()).runtime
	if _, err := NewOrphanedTurnRunEraser(db, nil, turns); !fault.HasCode(err, turn.ErrorCodeInvalidArgument) {
		t.Fatalf("NewOrphanedTurnRunEraser(db, nil, turns) = %v, want %s", err, turn.ErrorCodeInvalidArgument)
	}
	if _, err := NewOrphanedTurnRunEraser(db, runs, nil); !fault.HasCode(err, turn.ErrorCodeInvalidArgument) {
		t.Fatalf("NewOrphanedTurnRunEraser(db, runs, nil) = %v, want %s", err, turn.ErrorCodeInvalidArgument)
	}
}
