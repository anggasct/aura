package runtimeengine

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"testing"

	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/platform/turn/ingress"
	"github.com/anggasct/aura/internal/platform/turn/turntest"
	"github.com/anggasct/aura/internal/store"
)

type recordingHook struct {
	mu       sync.Mutex
	calls    int
	sessions []string
	owners   []string
	counts   []int
	err      error
}

func (h *recordingHook) fn(_ context.Context, req *turn.TurnRequest, events []store.RuntimeEvent) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	if req != nil {
		h.sessions = append(h.sessions, req.SessionID)
		h.owners = append(h.owners, req.PrincipalID)
	}
	h.counts = append(h.counts, len(events))
	return h.err
}

func hookMessageStep(text string) turntest.FakeStep {
	return turntest.FakeStep{Kind: turn.EventKindMessageCompleted, Payload: json.RawMessage(`{"text":` + strconv.Quote(text) + `}`)}
}

func TestPostTurnHookRunsAfterCommit(t *testing.T) {
	executor := turntest.NewFakeExecutor([]turntest.FakeStep{hookMessageStep("hello from model")})
	hook := &recordingHook{}
	engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4, PostTurnHook: hook.fn}, executor)
	mustCreateSession(t, db, "session-a")
	req := &turn.TurnRequest{
		TurnID: "turn-hook-1", SessionID: "session-a", PrincipalID: "user-1",
		Origin: turn.OriginTerminal, Parts: []runtimeingress.InputPart{{Text: "hello"}},
	}
	events, err := collect(t, engine, req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	terminal := events[len(events)-1].Kind
	if terminal != turn.EventKindTurnCompleted {
		t.Fatalf("terminal = %q, want turn.completed", terminal)
	}
	hook.mu.Lock()
	defer hook.mu.Unlock()
	if hook.calls != 1 {
		t.Fatalf("hook calls = %d, want 1", hook.calls)
	}
	if hook.sessions[0] != "session-a" || hook.owners[0] != "user-1" {
		t.Errorf("hook req = session %q owner %q, want session-a user-1", hook.sessions[0], hook.owners[0])
	}
	if hook.counts[0] != 1 {
		t.Errorf("hook events = %d, want 1 executed event", hook.counts[0])
	}
}

func TestPostTurnHookFailureDoesNotFailTurn(t *testing.T) {
	executor := turntest.NewFakeExecutor([]turntest.FakeStep{hookMessageStep("hello from model")})
	hook := &recordingHook{err: errors.New("projection boom")}
	engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4, PostTurnHook: hook.fn}, executor)
	mustCreateSession(t, db, "session-a")
	req := &turn.TurnRequest{
		TurnID: "turn-hook-2", SessionID: "session-a", PrincipalID: "user-1",
		Origin: turn.OriginTerminal, Parts: []runtimeingress.InputPart{{Text: "hello"}},
	}
	events, err := collect(t, engine, req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	terminal := events[len(events)-1].Kind
	if terminal != turn.EventKindTurnCompleted {
		t.Fatalf("terminal = %q, want turn.completed despite hook failure (fail-open)", terminal)
	}
	hook.mu.Lock()
	defer hook.mu.Unlock()
	if hook.calls != 1 {
		t.Fatalf("hook calls = %d, want 1", hook.calls)
	}
}
