package runtimeengine

import (
	"context"
	"database/sql"
	"testing"

	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/store"
)

func TestCancelActiveCancelsOnlyTheRunningTurn(t *testing.T) {
	modes := map[string]func(t *testing.T, cfg Config, executor TurnExecutor) (*Engine, *sql.DB){
		"in-process": func(t *testing.T, cfg Config, executor TurnExecutor) (*Engine, *sql.DB) {
			engine, db, _ := newTestRuntime(t, cfg, executor)
			return engine, db
		},
		"durable": func(t *testing.T, cfg Config, executor TurnExecutor) (*Engine, *sql.DB) {
			engine, _, db := newDurableLiveTestRuntime(t, cfg, executor)
			return engine, db
		},
	}
	for name, build := range modes {
		t.Run(name, func(t *testing.T) {
			gate := make(chan struct{})
			executor := runtime.NewFakeExecutor([]runtime.FakeStep{{Kind: runtime.EventKindModelStarted, Payload: []byte(`{}`), Block: gate}})
			engine, db := build(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 8}, executor)
			mustCreateSession(t, db, "session-a")
			mustCreateSession(t, db, "session-b")

			if id, cancelled, err := engine.CancelActive(t.Context(), "session-a"); err != nil || cancelled || id != "" {
				t.Fatalf("idle CancelActive = %q %v %v, want no active turn", id, cancelled, err)
			}
			if _, _, err := engine.CancelActive(t.Context(), ""); err == nil {
				t.Fatal("empty session id accepted")
			}

			results := make(chan []store.RuntimeEvent, 2)
			submit := func(id string) {
				go func() {
					events, _ := collect(t, engine, sampleRequest("session-a", id))
					results <- events
				}()
			}
			submit("turn-running")
			waitFor(t, func() bool { return executor.StartCount() == 1 })
			submit("turn-queued")
			waitFor(t, func() bool { _, queued := engine.SessionActivity("session-a"); return queued == 1 })
			active, queued := engine.SessionActivity("session-a")
			if active != "turn-running" || queued != 1 {
				t.Fatalf("SessionActivity = %q queued %d, want turn-running queued 1", active, queued)
			}
			if other, otherQueued := engine.SessionActivity("session-b"); other != "" || otherQueued != 0 {
				t.Fatalf("other session activity = %q %d", other, otherQueued)
			}
			if id, cancelled, err := engine.CancelActive(t.Context(), "session-b"); err != nil || cancelled || id != "" {
				t.Fatalf("other session CancelActive = %q %v %v", id, cancelled, err)
			}

			id, cancelled, err := engine.CancelActive(t.Context(), "session-a")
			if err != nil || !cancelled || id != "turn-running" {
				t.Fatalf("CancelActive = %q %v %v, want turn-running", id, cancelled, err)
			}
			waitForTerminalDurable(t, db, "turn-running")
			close(gate)
			terminals := map[string]string{}
			for range 2 {
				for _, ev := range <-results {
					if isTerminalKind(ev.Kind) {
						terminals[ev.TurnID] = ev.Kind
					}
				}
			}
			if terminals["turn-running"] != runtime.EventKindTurnCancelled {
				t.Errorf("running turn terminal = %q, want turn.cancelled", terminals["turn-running"])
			}
			if terminals["turn-queued"] != runtime.EventKindTurnCompleted {
				t.Errorf("queued turn terminal = %q, want turn.completed", terminals["turn-queued"])
			}
			waitFor(t, func() bool { active, queued := engine.SessionActivity("session-a"); return active == "" && queued == 0 })
		})
	}
}

func TestCancelActiveAfterCompletionReportsNoActiveTurn(t *testing.T) {
	executor := runtime.NewFakeExecutor([]runtime.FakeStep{{Kind: runtime.EventKindModelStarted, Payload: []byte(`{}`)}})
	engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, executor)
	mustCreateSession(t, db, "session-a")
	if _, err := collect(t, engine, sampleRequest("session-a", "turn-done")); err != nil {
		t.Fatalf("run: %v", err)
	}
	waitFor(t, func() bool { active, _ := engine.SessionActivity("session-a"); return active == "" })
	if id, cancelled, err := engine.CancelActive(context.Background(), "session-a"); err != nil || cancelled || id != "" {
		t.Fatalf("CancelActive = %q %v %v, want no active turn", id, cancelled, err)
	}
}
