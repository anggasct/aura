package runtimeadk

import (
	"context"
	"testing"

	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/platform/turn/engine"
	"github.com/anggasct/aura/internal/platform/turn/ingress"
	"github.com/anggasct/aura/internal/platform/turn/store"
)

func engineLimits(active, pending int) runtimeengine.Config {
	cfg := runtimeengine.Default()
	cfg.MaxActiveTurns, cfg.MaxPendingTurns = active, pending
	return cfg
}

func TestEnginePersistsADKEventFidelity(t *testing.T) {
	model := &fakeADKModel{answer: "engine answer", tokens: 4}
	modelName, models := fakeModelSet(model)
	broker := &fakeBroker{}
	db, sessions, events := newSessionTestDB(t)
	executor, err := NewADKExecutor("aura", modelName, models, sessions, broker, nil, nil)
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	dedupe := turnstore.NewDedupeStore(db)
	engine, err := runtimeengine.New(&runtimeengine.Deps{Events: events, Dedupe: dedupe, Executor: executor, Config: engineLimits(2, 4)})
	if err != nil {
		t.Fatalf("runtimeengine.New: %v", err)
	}
	mustCreateSession(t, db, "session-1")

	eventsCh, errs := collectStream(engine, &turn.TurnRequest{
		TurnID: "turn-1", SessionID: "session-1", PrincipalID: "user-1", Origin: turn.OriginTerminal,
		Parts: []runtimeingress.InputPart{{Text: "hi"}},
	})

	var adkCount int
	var sawInvocation bool
	for ev := range eventsCh {
		if ev.Kind == turnstore.EventKindADK {
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

	stored, err := sessions.ListEvents(context.Background(), "session-1", 0, 100)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var storedADK int
	for i := range stored {
		if stored[i].Kind == turnstore.EventKindADK {
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

func collectStream(engine *runtimeengine.Engine, req *turn.TurnRequest) (eventsCh <-chan turn.Event, errCh <-chan error) {
	events := make(chan turn.Event, 64)
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

func TestEngineSingleWriterForADKEvents(t *testing.T) {
	model := &fakeADKModel{answer: "single writer", tokens: 2}
	modelName, models := fakeModelSet(model)
	db, sessions, events := newSessionTestDB(t)
	executor, err := NewADKExecutor("aura", modelName, models, sessions, &fakeBroker{}, nil, nil)
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	dedupe := turnstore.NewDedupeStore(db)
	engine, err := runtimeengine.New(&runtimeengine.Deps{Events: events, Dedupe: dedupe, Executor: executor, Config: engineLimits(2, 4)})
	if err != nil {
		t.Fatalf("runtimeengine.New: %v", err)
	}
	mustCreateSession(t, db, "session-1")

	req := &turn.TurnRequest{
		TurnID: "turn-1", SessionID: "session-1", PrincipalID: "user-1", Origin: turn.OriginTerminal,
		Parts: []runtimeingress.InputPart{{Text: "hi"}},
	}
	var streamed []turn.Event
	for ev, err := range engine.Run(context.Background(), req) {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		streamed = append(streamed, ev)
	}

	stored, err := sessions.ListEvents(context.Background(), "session-1", 0, 1000)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var streamedADK, storedADK int
	seen := map[string]int{}
	for _, ev := range stored {
		if ev.Kind != turnstore.EventKindADK {
			continue
		}
		storedADK++
		seen[ev.ID]++
	}
	for _, ev := range streamed {
		if ev.Kind != turnstore.EventKindADK {
			continue
		}
		streamedADK++
		if seen[ev.ID] != 1 {
			t.Errorf("streamed ADK event %q has %d stored rows, want 1", ev.ID, seen[ev.ID])
		}
	}
	if streamedADK == 0 || storedADK == 0 {
		t.Fatalf("streamed=%d stored=%d, want both non-zero", streamedADK, storedADK)
	}
	if storedADK != streamedADK {
		t.Errorf("stored ADK rows = %d, streamed ADK events = %d — single writer required", storedADK, streamedADK)
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("ADK event %q stored %d times, want 1", id, n)
		}
	}
}
