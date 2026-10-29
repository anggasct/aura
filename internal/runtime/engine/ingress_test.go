package runtimeengine

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"testing/synctest"
	"time"

	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/runtime/ingress"
)

func terminalCount(t *testing.T, db *sql.DB, turnID string) int {
	t.Helper()
	var count int
	err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM runtime_event WHERE turn_id = ? AND kind IN (?, ?, ?)`,
		turnID, runtime.EventKindTurnCompleted, runtime.EventKindTurnFailed, runtime.EventKindTurnCancelled).Scan(&count)
	if err != nil {
		t.Fatalf("count terminal: %v", err)
	}
	return count
}

func eventCountByKind(t *testing.T, db *sql.DB, turnID, kind string) int {
	t.Helper()
	var count int
	err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM runtime_event WHERE turn_id = ? AND kind = ?`,
		turnID, kind).Scan(&count)
	if err != nil {
		t.Fatalf("count %s: %v", kind, err)
	}
	return count
}

func jsonStep(kind string) runtime.FakeStep {
	return runtime.FakeStep{Kind: kind, Payload: json.RawMessage(`{}`)}
}

func sampleSession(conversation string) string {
	return runtimeingress.SessionKey("main", "telegram", conversation)
}

func sampleEnvelope(conversation, externalID string) *runtimeingress.IngressEnvelope {
	return &runtimeingress.IngressEnvelope{
		Source:         "telegram",
		AgentID:        "main",
		ExternalID:     externalID,
		PrincipalID:    "user-1",
		ConversationID: sampleSession(conversation),
		Parts:          []runtimeingress.InputPart{{Text: "hello"}},
		ReceivedAt:     time.Now().UTC(),
	}
}

func TestAcceptRunsTurnToDurableTerminal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := runtime.NewFakeExecutor([]runtime.FakeStep{jsonStep(runtime.EventKindModelStarted), jsonStep(runtime.EventKindMessageCompleted)})
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, executor)
		mustCreateSession(t, db, sampleSession("conv-1"))

		ref, err := engine.Accept(context.Background(), sampleEnvelope("conv-1", "msg-1"))
		if err != nil {
			t.Fatalf("Accept: %v", err)
		}
		if ref.Replayed {
			t.Error("first delivery must not be a replay")
		}
		if ref.SessionID != sampleSession("conv-1") || ref.TurnID == "" {
			t.Errorf("unexpected turn ref: %+v", ref)
		}

		waitFor(t, func() bool { return terminalCount(t, db, ref.TurnID) == 1 })
		if got := eventCountByKind(t, db, ref.TurnID, runtime.EventKindTurnAccepted); got != 1 {
			t.Errorf("accepted events = %d, want 1", got)
		}
		if got := eventCountByKind(t, db, ref.TurnID, runtime.EventKindTurnCompleted); got != 1 {
			t.Errorf("completed events = %d, want 1", got)
		}
	})
}

func TestAcceptDuplicateReturnsOriginal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := runtime.NewFakeExecutor([]runtime.FakeStep{jsonStep(runtime.EventKindMessageCompleted)})
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, executor)
		mustCreateSession(t, db, sampleSession("conv-1"))

		first, err := engine.Accept(context.Background(), sampleEnvelope("conv-1", "msg-dup"))
		if err != nil {
			t.Fatalf("first Accept: %v", err)
		}
		second, err := engine.Accept(context.Background(), sampleEnvelope("conv-1", "msg-dup"))
		if err != nil {
			t.Fatalf("second Accept: %v", err)
		}
		if !second.Replayed {
			t.Error("duplicate delivery must be flagged replayed")
		}
		if second.TurnID != first.TurnID {
			t.Errorf("duplicate turn = %q, want original %q", second.TurnID, first.TurnID)
		}

		waitFor(t, func() bool { return terminalCount(t, db, first.TurnID) == 1 })
		var totalAccepted int
		if err := db.QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM runtime_event WHERE kind = ?`, runtime.EventKindTurnAccepted).Scan(&totalAccepted); err != nil {
			t.Fatalf("count accepted: %v", err)
		}
		if totalAccepted != 1 {
			t.Errorf("total accepted events = %d, want 1 (no second sequence)", totalAccepted)
		}
	})
}

func TestAcceptAfterShutdownIsRejected(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := runtime.NewFakeExecutor([]runtime.FakeStep{jsonStep(runtime.EventKindMessageCompleted)})
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, executor)
		mustCreateSession(t, db, sampleSession("conv-1"))

		if err := engine.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		_, err := engine.Accept(context.Background(), sampleEnvelope("conv-1", "msg-late"))
		code, ok := runtime.CodeOf(err)
		if !ok || code != runtime.ErrorCodeRuntimeOverloaded {
			t.Fatalf("Accept after shutdown code = %q (ok=%v), want runtime_overloaded", code, ok)
		}
	})
}

func TestAcceptValidatesIdentity(t *testing.T) {
	executor := runtime.NewFakeExecutor(nil)
	engine, db, _ := newTestRuntime(t, Config{}, executor)
	mustCreateSession(t, db, sampleSession("conv-1"))

	cases := map[string]*runtimeingress.IngressEnvelope{
		"nil envelope":          nil,
		"missing source":        {AgentID: "main", ExternalID: "x", PrincipalID: "u", ConversationID: sampleSession("conv-1")},
		"missing external":      {Source: "telegram", AgentID: "main", PrincipalID: "u", ConversationID: sampleSession("conv-1")},
		"missing principal":     {Source: "telegram", AgentID: "main", ExternalID: "x", ConversationID: sampleSession("conv-1")},
		"missing conversation":  {Source: "telegram", AgentID: "main", ExternalID: "x", PrincipalID: "u"},
		"missing persona":       {Source: "telegram", ExternalID: "x", PrincipalID: "u", ConversationID: sampleSession("conv-1")},
		"unscoped conversation": {Source: "telegram", AgentID: "main", ExternalID: "x", PrincipalID: "u", ConversationID: "conv-1"},
		"other persona scope":   {Source: "telegram", AgentID: "kuma", ExternalID: "x", PrincipalID: "u", ConversationID: sampleSession("conv-1")},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := engine.Accept(context.Background(), env)
			code, ok := runtime.CodeOf(err)
			if !ok || code != runtime.ErrorCodeInvalidArgument {
				t.Fatalf("code = %q (ok=%v), want invalid_argument", code, ok)
			}
		})
	}
}
