package runtimeengine

import (
	"context"
	"encoding/json"
	"iter"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/store"
)

type twoInvocationExecutor struct {
	mu        sync.Mutex
	deadlines []time.Time
	blocked   chan struct{}
}

func newTwoInvocationExecutor() *twoInvocationExecutor {
	return &twoInvocationExecutor{blocked: make(chan struct{})}
}

func (x *twoInvocationExecutor) Execute(ctx context.Context, req *turn.TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
	return func(yield func(store.RuntimeEvent, error) bool) {
		deadline, _ := ctx.Deadline()
		x.mu.Lock()
		x.deadlines = append(x.deadlines, deadline)
		first := len(x.deadlines) == 1
		x.mu.Unlock()
		if first {
			close(x.blocked)
			<-ctx.Done()
			yield(store.RuntimeEvent{}, ctx.Err())
			return
		}
		yield(store.RuntimeEvent{ID: req.TurnID + "-done", Kind: turn.EventKindMessageCompleted, Payload: []byte(`{"text":"ok"}`), CreatedAt: time.Now().UTC()}, nil)
	}
}

func (x *twoInvocationExecutor) seenDeadlines() []time.Time {
	x.mu.Lock()
	defer x.mu.Unlock()
	return append([]time.Time(nil), x.deadlines...)
}

func (x *twoInvocationExecutor) waitBlocked(t *testing.T) {
	t.Helper()
	select {
	case <-x.blocked:
	case <-time.After(hostRigTimeout):
		t.Fatal("the first invocation never reached the executor")
	}
}

func runAcrossALostLease(t *testing.T, req *turn.TurnRequest) (*durableHostRig, []time.Time) {
	t.Helper()
	executor := newTwoInvocationExecutor()
	rig := newShortLeaseHostRig(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16, TurnTimeout: time.Hour}, executor)
	mustCreateSession(t, rig.db, req.SessionID)
	rig.startHost()
	outcome := callTurn(context.WithoutCancel(t.Context()), rig.engine, req)
	executor.waitBlocked(t)
	time.Sleep(20 * time.Millisecond)
	loseHostLease(t, rig.db)
	awaitTerminal(t, outcome, turn.EventKindTurnCompleted)
	deadlines := executor.seenDeadlines()
	if len(deadlines) != 2 {
		t.Fatalf("executor invocations = %d, want 2", len(deadlines))
	}
	return rig, deadlines
}

func turnStartRows(t *testing.T, rig *durableHostRig, turnID string) int {
	t.Helper()
	var rows int
	if err := rig.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM durable_action WHERE run_key = ? AND action_key = 'turn-start'`, turnID).Scan(&rows); err != nil {
		t.Fatalf("count the start rows of %s: %v", turnID, err)
	}
	return rows
}

func runInBubble(engine *Engine, req *turn.TurnRequest) <-chan []store.RuntimeEvent {
	out := make(chan []store.RuntimeEvent, 1)
	go func() {
		var events []store.RuntimeEvent
		for ev, err := range engine.Run(context.Background(), req) {
			if err != nil {
				break
			}
			events = append(events, ev)
		}
		out <- events
	}()
	return out
}

func closeInBubble(t *testing.T, engine *Engine) {
	t.Helper()
	if err := engine.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	if backend, ok := engine.durableRuntime.(*durable.Fake); ok {
		backend.Close()
	}
	synctest.Wait()
}

func TestDurableTurnTimeoutCountsFromItsFirstStart(t *testing.T) {
	t.Run("a turn that waited behind another keeps its whole timeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			executor := turn.NewFakeExecutor([]turn.FakeStep{
				{Kind: turn.EventKindModelStarted, Payload: []byte(`{}`), Wait: 40 * time.Second},
				jsonStep(turn.EventKindMessageCompleted),
			})
			engine, _, db := newDurableLiveTestRuntime(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16, TurnTimeout: time.Minute}, executor)
			mustCreateSession(t, db, "session-a")
			var streams []<-chan []store.RuntimeEvent
			for i := range 2 {
				streams = append(streams, runInBubble(engine, sampleRequest("session-a", turnID(i))))
				synctest.Wait()
			}
			for i, stream := range streams {
				if got := lastKind(<-stream); got != turn.EventKindTurnCompleted {
					t.Errorf("turn %d ended with %q, want turn.completed", i, got)
				}
			}
			closeInBubble(t, engine)
		})
	})
	t.Run("a turn that runs longer than the timeout fails", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			executor := turn.NewFakeExecutor([]turn.FakeStep{
				{Kind: turn.EventKindModelStarted, Payload: []byte(`{}`), Wait: 2 * time.Minute},
				jsonStep(turn.EventKindMessageCompleted),
			})
			engine, _, db := newDurableLiveTestRuntime(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16, TurnTimeout: time.Minute}, executor)
			mustCreateSession(t, db, "session-a")
			events := <-runInBubble(engine, sampleRequest("session-a", turnID(0)))
			if got := lastKind(events); got != turn.EventKindTurnFailed {
				t.Fatalf("turn ended with %q, want turn.failed", got)
			}
			var payload struct {
				Code turn.ErrorCode `json:"code"`
			}
			if err := json.Unmarshal(events[len(events)-1].Payload, &payload); err != nil || payload.Code != turn.ErrorCodeTurnDeadlineExceeded {
				t.Errorf("terminal code = %q, %v; want turn_deadline_exceeded", payload.Code, err)
			}
			closeInBubble(t, engine)
		})
	})
}

func TestDurableTurnDeadlineIsTheSameOnEveryInvocation(t *testing.T) {
	t.Run("the start is journaled once", func(t *testing.T) {
		before := time.Now()
		rig, deadlines := runAcrossALostLease(t, sampleRequest("session-a", "turn-1"))
		if !deadlines[0].Equal(deadlines[1]) {
			t.Errorf("deadlines = %v and %v, want the same on both invocations", deadlines[0], deadlines[1])
		}
		if deadlines[0].Before(before.Add(time.Hour)) || deadlines[0].After(time.Now().Add(time.Hour)) {
			t.Errorf("deadline = %v, want the first start plus the turn timeout", deadlines[0])
		}
		if rows := turnStartRows(t, rig, "turn-1"); rows != 1 {
			t.Errorf("turn-start rows = %d, want 1", rows)
		}
	})
	t.Run("a request's own deadline records no start", func(t *testing.T) {
		own := time.Now().Add(30 * time.Minute).UTC().Truncate(time.Millisecond)
		req := sampleRequest("session-a", "turn-1")
		req.Deadline = own
		rig, deadlines := runAcrossALostLease(t, req)
		for i, deadline := range deadlines {
			if !deadline.Equal(own) {
				t.Errorf("deadline of invocation %d = %v, want the request's %v", i+1, deadline, own)
			}
		}
		if rows := turnStartRows(t, rig, "turn-1"); rows != 0 {
			t.Errorf("turn-start rows = %d, want 0", rows)
		}
	})
}
