package runtimeengine

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"testing/synctest"

	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/store"
)

func TestTurnEventsCarryAgentID(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := runtime.NewFakeExecutor([]runtime.FakeStep{
			{Kind: runtime.EventKindMessageCompleted},
		})
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 8, DefaultAgentID: "main"}, executor)
		mustCreateSession(t, db, "session-agent")

		events, err := collect(t, engine, sampleRequest("session-agent", "turn-agent"))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		accepted := eventByKind(t, events, runtime.EventKindTurnAccepted)
		if got := jsonStringField(t, accepted.Payload, "agent_id"); got != "main" {
			t.Fatalf("accepted agent_id = %q, want main from the engine default", got)
		}
		if err := engine.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		completed := terminalPayload(t, db, "turn-agent")
		if got := jsonStringField(t, completed, "agent_id"); got != "main" {
			t.Fatalf("terminal agent_id = %q, want main", got)
		}

		requested := sampleRequest("session-agent", "turn-agent-explicit")
		requested.AgentID = "reviewer"
		engine2, db2, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 8, DefaultAgentID: "main"}, executor)
		mustCreateSession(t, db2, "session-agent")
		if _, err := collect(t, engine2, requested); err != nil {
			t.Fatalf("Run explicit: %v", err)
		}
		if err := engine2.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown explicit: %v", err)
		}
		payload := terminalPayload(t, db2, "turn-agent-explicit")
		if got := jsonStringField(t, payload, "agent_id"); got != "reviewer" {
			t.Fatalf("terminal agent_id = %q, want the requested reviewer", got)
		}
	})
}

func terminalPayload(t *testing.T, db *sql.DB, turnID string) []byte {
	t.Helper()
	var payload []byte
	if err := db.QueryRowContext(context.Background(),
		`SELECT payload_json FROM runtime_event WHERE turn_id = ? AND kind IN (?, ?, ?)`,
		turnID, runtime.EventKindTurnCompleted, runtime.EventKindTurnFailed, runtime.EventKindTurnCancelled,
	).Scan(&payload); err != nil {
		t.Fatalf("load terminal event for %s: %v", turnID, err)
	}
	return payload
}

func eventByKind(t *testing.T, events []store.RuntimeEvent, kind string) store.RuntimeEvent {
	t.Helper()
	for i := range events {
		if events[i].Kind == kind {
			return events[i]
		}
	}
	t.Fatalf("no %s event among %d events", kind, len(events))
	return store.RuntimeEvent{}
}

func jsonStringField(t *testing.T, payload []byte, key string) string {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode payload %s: %v", payload, err)
	}
	value, _ := decoded[key].(string)
	return value
}
