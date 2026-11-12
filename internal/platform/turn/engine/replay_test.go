package runtimeengine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"iter"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/store"
)

var replayEventKinds = []string{turn.EventKindModelStarted, turn.EventKindModelStarted, turn.EventKindMessageCompleted}

type replayingExecutor struct {
	mu          sync.Mutex
	invocations int
	yielded     map[int][]store.RuntimeEvent
	blocked     chan struct{}
	replayErr   error
}

func newReplayingExecutor(replayErr error) *replayingExecutor {
	return &replayingExecutor{yielded: map[int][]store.RuntimeEvent{}, blocked: make(chan struct{}), replayErr: replayErr}
}

func (x *replayingExecutor) Execute(ctx context.Context, _ *turn.TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
	return func(yield func(store.RuntimeEvent, error) bool) {
		x.mu.Lock()
		x.invocations++
		invocation := x.invocations
		x.mu.Unlock()
		if invocation > 1 && x.replayErr != nil {
			yield(store.RuntimeEvent{}, x.replayErr)
			return
		}
		for position, kind := range replayEventKinds {
			if invocation == 1 && position == 1 && !yield(streamedOnlyOnce(), nil) {
				return
			}
			if invocation == 1 && position == 2 {
				close(x.blocked)
				<-ctx.Done()
				yield(store.RuntimeEvent{}, ctx.Err())
				return
			}
			ev := store.RuntimeEvent{
				ID:        NewTurnID(),
				Kind:      kind,
				Payload:   fmt.Appendf(nil, `{"invocation":%d,"position":%d}`, invocation, position+1),
				CreatedAt: time.Now().UTC(),
			}
			x.mu.Lock()
			x.yielded[invocation] = append(x.yielded[invocation], ev)
			x.mu.Unlock()
			if !yield(ev, nil) {
				return
			}
		}
	}
}

func streamedOnlyOnce() store.RuntimeEvent {
	return store.RuntimeEvent{
		ID:        NewTurnID(),
		Kind:      store.EventKindADK,
		Payload:   adkPartialStep("streamed by the first invocation only").Payload,
		CreatedAt: time.Now().UTC(),
	}
}

func (x *replayingExecutor) invocationCount() int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.invocations
}

func (x *replayingExecutor) yieldedBy(invocation int) []store.RuntimeEvent {
	x.mu.Lock()
	defer x.mu.Unlock()
	return slices.Clone(x.yielded[invocation])
}

func (x *replayingExecutor) waitBlocked(t *testing.T) {
	t.Helper()
	select {
	case <-x.blocked:
	case <-time.After(hostRigTimeout):
		t.Fatal("the first invocation never reached its block")
	}
}

func turnEventsOf(t *testing.T, db *sql.DB, turnID string) []store.RuntimeEvent {
	t.Helper()
	events, err := store.NewDedupeStore(db).ListTurnEvents(t.Context(), acceptedSessionOf(t, db, turnID), turnID)
	if err != nil {
		t.Fatalf("list the events of %s: %v", turnID, err)
	}
	return events
}

func executorEventsOf(t *testing.T, db *sql.DB, turnID string) []store.RuntimeEvent {
	t.Helper()
	events := turnEventsOf(t, db, turnID)
	var out []store.RuntimeEvent
	for i := range events {
		if slices.Contains(replayEventKinds, events[i].Kind) {
			out = append(out, events[i])
		}
	}
	return out
}

func terminalEventsOf(t *testing.T, db *sql.DB, turnID string) []store.RuntimeEvent {
	t.Helper()
	events := turnEventsOf(t, db, turnID)
	var out []store.RuntimeEvent
	for i := range events {
		if slices.Contains(terminalKinds, events[i].Kind) {
			out = append(out, events[i])
		}
	}
	return out
}

func invocationOf(t *testing.T, ev *store.RuntimeEvent) int {
	t.Helper()
	var decoded struct {
		Invocation int `json:"invocation"`
	}
	if err := json.Unmarshal(ev.Payload, &decoded); err != nil {
		t.Fatalf("decode the payload of %s: %v", ev.ID, err)
	}
	return decoded.Invocation
}

func loseHostLease(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `DELETE FROM durable_host`); err != nil {
		t.Fatalf("delete the host lease: %v", err)
	}
}

func startBlockedReplayTurn(t *testing.T, executor *replayingExecutor) (rig *durableHostRig, outcome <-chan turnOutcome) {
	t.Helper()
	cfg := Config{MaxActiveTurns: 4, MaxPendingTurns: 16, Skills: &stubSkillRenderer{body: "Do things."}}
	rig = newShortLeaseHostRig(t, cfg, executor)
	mustCreateSession(t, rig.db, "session-a")
	rig.startHost()
	outcome = callTurn(context.WithoutCancel(t.Context()), rig.engine, pinnedRequest("session-a", "turn-1"))
	executor.waitBlocked(t)
	return rig, outcome
}

func TestReplayedTurnStoresItsEventsOnce(t *testing.T) {
	executor := newReplayingExecutor(nil)
	rig, outcome := startBlockedReplayTurn(t, executor)
	before := executorEventsOf(t, rig.db, "turn-1")
	if len(before) != 2 {
		t.Fatalf("events stored by the first invocation = %d, want 2", len(before))
	}

	loseHostLease(t, rig.db)
	awaitTerminal(t, outcome, turn.EventKindTurnCompleted)

	if n := executor.invocationCount(); n != 2 {
		t.Fatalf("executor invocations = %d, want 2", n)
	}
	stored := executorEventsOf(t, rig.db, "turn-1")
	if len(stored) != 3 {
		t.Fatalf("stored executor events = %d, want 3", len(stored))
	}
	if ids := eventIDs(stored); !slices.Equal(ids, []string{"turn-1-ev-2", "turn-1-ev-3", "turn-1-ev-4"}) {
		t.Errorf("stored executor events = %v, want the positions after the activation", ids)
	}
	all := turnEventsOf(t, rig.db, "turn-1")
	if ids := idsOfKind(all, turn.EventKindSkillActivated); !slices.Equal(ids, []string{"turn-1-ev-1"}) {
		t.Errorf("stored skill activations = %v, want the first invocation's one", ids)
	}
	if ids := idsOfKind(all, store.EventKindADK); len(ids) != 0 {
		t.Errorf("stored streamed chunks = %v, want none", ids)
	}
	for i := range before {
		if stored[i].Sequence != before[i].Sequence || !stored[i].CreatedAt.Equal(before[i].CreatedAt) {
			t.Errorf("event %d = sequence %d at %s, want the first invocation's sequence %d at %s",
				i+1, stored[i].Sequence, stored[i].CreatedAt, before[i].Sequence, before[i].CreatedAt)
		}
		if got := invocationOf(t, &stored[i]); got != 1 {
			t.Errorf("event %d payload names invocation %d, want 1", i+1, got)
		}
	}
	if got := invocationOf(t, &stored[2]); got != 2 {
		t.Errorf("event 3 payload names invocation %d, want 2", got)
	}
	replayed := executor.yieldedBy(2)
	if len(replayed) != 3 {
		t.Fatalf("second invocation yielded %d events, want 3", len(replayed))
	}
	if replayed[0].CreatedAt.Equal(stored[0].CreatedAt) {
		t.Errorf("second invocation yielded its first event at the stored time %s, want a time of its own", stored[0].CreatedAt)
	}
}

func TestReplayThatFailsEarlyStillEndsTheTurn(t *testing.T) {
	executor := newReplayingExecutor(codedError(turn.ErrorCodePersonaNotFound, "agent definition no longer resolves", nil))
	rig, _ := startBlockedReplayTurn(t, executor)

	loseHostLease(t, rig.db)

	if state := waitRunEnded(t, rig.runtime, "turn-1"); state != durable.RunSucceeded {
		t.Fatalf("run of turn-1 = %s, want succeeded", state)
	}
	if n := executor.invocationCount(); n != 2 {
		t.Fatalf("executor invocations = %d, want 2", n)
	}
	terminals := terminalEventsOf(t, rig.db, "turn-1")
	if len(terminals) != 1 || terminals[0].Kind != turn.EventKindTurnFailed || terminals[0].ID != "turn-1-terminal" {
		t.Fatalf("terminal events of turn-1 = %+v, want one turn.failed with the id turn-1-terminal", terminals)
	}
	var payload struct {
		Code turn.ErrorCode `json:"code"`
	}
	if err := json.Unmarshal(terminals[0].Payload, &payload); err != nil {
		t.Fatalf("decode the terminal payload: %v", err)
	}
	if payload.Code != turn.ErrorCodePersonaNotFound {
		t.Errorf("terminal code = %q, want %q", payload.Code, turn.ErrorCodePersonaNotFound)
	}
	stored := executorEventsOf(t, rig.db, "turn-1")
	if ids := eventIDs(stored); !slices.Equal(ids, []string{"turn-1-ev-2", "turn-1-ev-3"}) {
		t.Errorf("stored executor events = %v, want the first invocation's two", ids)
	}
	if ids := idsOfKind(turnEventsOf(t, rig.db, "turn-1"), turn.EventKindSkillActivated); !slices.Equal(ids, []string{"turn-1-ev-1"}) {
		t.Errorf("stored skill activations = %v, want the first invocation's one", ids)
	}
	if active, _ := storeActiveTurn(t, rig.sessions, "session-a"); active != "" {
		t.Errorf("active turn of session-a = %q, want the session released", active)
	}
}

func TestDurableTurnTerminalEventHasAFixedID(t *testing.T) {
	rig := newDurableHostRig(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, newHoldingExecutor())
	mustCreateSession(t, rig.db, "session-a")
	rig.startHost()

	awaitTerminal(t, callTurn(context.WithoutCancel(t.Context()), rig.engine, sampleRequest("session-a", "turn-1")), turn.EventKindTurnCompleted)

	terminals := terminalEventsOf(t, rig.db, "turn-1")
	if len(terminals) != 1 || terminals[0].ID != "turn-1-terminal" {
		t.Fatalf("terminal events of turn-1 = %v, want one with the id turn-1-terminal", eventIDs(terminals))
	}
}

func idsOfKind(events []store.RuntimeEvent, kind string) []string {
	var ids []string
	for i := range events {
		if events[i].Kind == kind {
			ids = append(ids, events[i].ID)
		}
	}
	return ids
}

func eventIDs(events []store.RuntimeEvent) []string {
	ids := make([]string, 0, len(events))
	for i := range events {
		ids = append(ids, events[i].ID)
	}
	return ids
}
