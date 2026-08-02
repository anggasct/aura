package runtime

import (
	"context"
	"testing"

	"github.com/anggasct/aura/internal/store"
)

// The engine must persist ADK executor events with their full fidelity:
// invocation and branch survive the queue's persist path, not just kind and
// payload.
func TestEnginePersistsADKEventFidelity(t *testing.T) {
	model := &fakeADKModel{answer: "engine answer", tokens: 4}
	modelName := registerFakeModel(t, model)
	broker := &fakeBroker{}
	db, sessions, events := newSessionTestDB(t)
	executor, err := NewADKExecutor("aura", modelName, sessions, events, broker, nil, nil)
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	dedupe := store.NewDedupeStore(db)
	engine, err := NewEngine(Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, events, dedupe, executor, nil)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	mustCreateSession(t, db, "session-1")

	eventsCh, errs := collectStream(engine, &TurnRequest{
		TurnID: "turn-1", SessionID: "session-1", PrincipalID: "user-1", Origin: OriginTerminal,
		Parts: []InputPart{{Text: "hi"}},
	})

	var adkCount int
	var sawInvocation bool
	for ev := range eventsCh {
		if ev.Kind == store.EventKindADK {
			adkCount++
			if ev.InvocationID != "" {
				sawInvocation = true
			}
			if ev.TurnID != "turn-1" || ev.SessionID != "session-1" {
				t.Errorf("event stamped wrong identity: turn=%q session=%q", ev.TurnID, ev.SessionID)
			}
		}
	}
	if err := <-errs; err != nil {
		t.Fatalf("stream: %v", err)
	}
	if adkCount == 0 {
		t.Fatal("no ADK events streamed")
	}
	if !sawInvocation {
		t.Error("persisted ADK events lost their invocation id")
	}

	// The stored log must hold the same fidelity.
	stored, err := sessions.ListEvents(context.Background(), "session-1", 0, 100)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var storedADK int
	for i := range stored {
		if stored[i].Kind == store.EventKindADK {
			storedADK++
			if stored[i].InvocationID == "" {
				t.Error("stored ADK event lost its invocation id")
			}
		}
	}
	if storedADK == 0 {
		t.Error("no ADK events stored")
	}
}

// collectStream drains an engine Run stream into a channel.
func collectStream(engine *Engine, req *TurnRequest) (eventsCh <-chan store.RuntimeEvent, errCh <-chan error) {
	events := make(chan store.RuntimeEvent, 64)
	errs := make(chan error, 1)
	go func() {
		for ev, err := range engine.Run(context.Background(), req) {
			if err != nil {
				errs <- err
				close(events)
				return
			}
			events <- ev
		}
		close(events)
		errs <- nil
	}()
	return events, errs
}
