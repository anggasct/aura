package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"iter"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/anggasct/aura/internal/store"
)

func newTestRuntime(t *testing.T, cfg Config, executor TurnExecutor) (*Engine, *sql.DB, store.EventStore) {
	t.Helper()
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "aura.db")
	db, err := store.OpenDB(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	events := store.NewEventStore(db)
	dedupe := store.NewDedupeStore(db)
	engine, err := NewEngine(cfg, events, dedupe, executor, nil)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return engine, db, events
}

func mustCreateSession(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	sess := store.Session{ID: id, OwnerID: "user-1"}
	if err := store.NewSessionService(db).Create(context.Background(), &sess); err != nil {
		t.Fatalf("create session %s: %v", id, err)
	}
}

func collect(t *testing.T, engine *Engine, req *TurnRequest) ([]store.RuntimeEvent, error) {
	t.Helper()
	var events []store.RuntimeEvent
	for ev, err := range engine.Run(context.Background(), req) {
		if err != nil {
			return events, err
		}
		events = append(events, ev)
	}
	return events, nil
}

func sampleRequest(sessionID, turnID string) *TurnRequest {
	return &TurnRequest{
		TurnID:      turnID,
		SessionID:   sessionID,
		PrincipalID: "user-1",
		Origin:      OriginTerminal,
		Parts:       []InputPart{{Text: "hello"}},
	}
}

func TestPerSessionFIFONoOverlap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := make(chan struct{})
		executor := NewFakeExecutor([]FakeStep{
			{Kind: EventKindModelStarted, Block: gate},
		})
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
		mustCreateSession(t, db, "session-a")

		// Submit serially, waiting for each accepted event to be durable, so the
		// submission order is deterministic and the queue preserves it.
		errs := make(chan error, 3)
		submit := func(n int) {
			go func() {
				_, err := collect(t, engine, sampleRequest("session-a", turnID(n)))
				errs <- err
			}()
			waitFor(t, func() bool { return acceptedCount(t, db, "session-a") > n })
		}
		for i := range 3 {
			submit(i)
		}

		// The fake executor blocks on gate, so only one turn can be active at a
		// time; the others must stay queued behind it.
		waitFor(t, func() bool { return executor.StartCount() == 1 })
		time.Sleep(50 * time.Millisecond)
		if got := executor.StartCount(); got != 1 {
			t.Fatalf("StartCount = %d, want 1 (single active turn per session)", got)
		}
		close(gate)
		waitFor(t, func() bool { return executor.StartCount() == 3 })
		for i := range 3 {
			if err := <-errs; err != nil {
				t.Fatalf("turn %d: %v", i, err)
			}
		}
		if got := executor.StartOrder(); got[0] != turnID(0) || got[1] != turnID(1) || got[2] != turnID(2) {
			t.Fatalf("StartOrder = %v, want FIFO order", got)
		}
	})
}

func TestGlobalConcurrencyLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := make(chan struct{})
		executor := NewFakeExecutor([]FakeStep{
			{Kind: EventKindModelStarted, Block: gate},
		})
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 16}, executor)
		for i := range 4 {
			mustCreateSession(t, db, "session-"+string(rune('a'+i)))
		}

		errs := make(chan error, 4)
		for i := range 4 {
			go func(n int) {
				_, err := collect(t, engine, sampleRequest("session-"+string(rune('a'+n)), turnID(n)))
				errs <- err
			}(i)
		}

		// Two sessions may run concurrently, but the global cap is two.
		waitFor(t, func() bool { return executor.StartCount() == 2 })
		time.Sleep(50 * time.Millisecond)
		if got := executor.StartCount(); got != 2 {
			t.Fatalf("StartCount = %d, want 2 (global active cap)", got)
		}
		close(gate)
		waitFor(t, func() bool { return executor.StartCount() == 4 })
		for i := range 4 {
			if err := <-errs; err != nil {
				t.Fatalf("turn %d: %v", i, err)
			}
		}
	})
}

func TestDuplicateIngressReturnsOriginalTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := NewFakeExecutor([]FakeStep{
			{Kind: EventKindModelStarted},
			{Kind: EventKindMessageCompleted},
		})
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
		mustCreateSession(t, db, "session-a")

		req := sampleRequest("session-a", "turn-dup")
		req.IdempotencyKey = "ingress-1"
		first, err := collect(t, engine, req)
		if err != nil {
			t.Fatalf("first run: %v", err)
		}

		second, err := collect(t, engine, req)
		if err != nil {
			t.Fatalf("duplicate run: %v", err)
		}
		_ = second

		if len(second) == 0 || second[0].TurnID != first[0].TurnID {
			t.Fatalf("duplicate turn id = %q, want original %q", firstEventTurn(second), firstEventTurn(first))
		}
		if firstEventTurn(second) != firstEventTurn(first) {
			t.Fatalf("duplicate replayed a different turn: %q vs %q", firstEventTurn(second), firstEventTurn(first))
		}

		// The duplicate must not have created a second event sequence.
		var count int
		if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM runtime_event WHERE turn_id = ?`, first[0].TurnID).Scan(&count); err != nil {
			t.Fatalf("count events: %v", err)
		}
		if count != len(first) {
			t.Errorf("stored events = %d, want %d (no second sequence)", count, len(first))
		}
		if executor.StartCount() != 1 {
			t.Errorf("executor started %d turns, want 1 (duplicate must not re-execute)", executor.StartCount())
		}
	})
}

func acceptedCount(t *testing.T, db *sql.DB, sessionID string) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM runtime_event WHERE session_id = ? AND kind = ?`,
		sessionID, EventKindTurnAccepted).Scan(&count); err != nil {
		t.Fatalf("count accepted: %v", err)
	}
	return count
}

func firstEventTurn(events []store.RuntimeEvent) string {
	for i := range events {
		if events[i].TurnID != "" {
			return events[i].TurnID
		}
	}
	return ""
}

func TestQueueOverflowIsTyped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := make(chan struct{})
		executor := NewFakeExecutor([]FakeStep{{Kind: EventKindModelStarted, Block: gate}})
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 1, MaxPendingTurns: 1}, executor)
		mustCreateSession(t, db, "session-a")
		mustCreateSession(t, db, "session-b")
		mustCreateSession(t, db, "session-c")

		errs := make(chan error, 2)
		// One turn occupies the active slot; one more fills the pending slot.
		go func() {
			_, err := collect(t, engine, sampleRequest("session-a", turnID(0)))
			errs <- err
		}()
		waitFor(t, func() bool { return acceptedCount(t, db, "session-a") >= 1 })
		go func() {
			_, err := collect(t, engine, sampleRequest("session-b", turnID(1)))
			errs <- err
		}()
		waitFor(t, func() bool { return acceptedCount(t, db, "session-b") >= 1 })

		// The third turn has no room: active slot busy, pending slot full.
		_, err := collect(t, engine, sampleRequest("session-c", turnID(2)))
		code, ok := CodeOf(err)
		if !ok || code != ErrorCodeRuntimeOverloaded {
			t.Fatalf("CodeOf(%v) = %q, %v; want runtime_overloaded", err, code, ok)
		}
		close(gate)
		for range 2 {
			if err := <-errs; err != nil {
				t.Fatalf("drained turn: %v", err)
			}
		}
	})
}

func turnID(n int) string {
	return "turn-" + string(rune('0'+n))
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within 5s")
}

func TestTurnDeadlineIsDurable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := make(chan struct{})
		executor := NewFakeExecutor([]FakeStep{{Kind: EventKindModelStarted, Block: gate}})
		engine, db, _ := newTestRuntime(t, Config{
			MaxActiveTurns: 2, MaxPendingTurns: 4, TurnTimeout: 50 * time.Millisecond,
		}, executor)
		mustCreateSession(t, db, "session-a")

		errs := make(chan error, 1)
		go func() {
			events, err := collect(t, engine, sampleRequest("session-a", "turn-deadline"))
			for _, ev := range events {
				if ev.Kind == EventKindTurnFailed {
					var payload struct {
						Code ErrorCode `json:"code"`
					}
					if err := json.Unmarshal(ev.Payload, &payload); err == nil && payload.Code == ErrorCodeTurnDeadlineExceeded {
						errs <- nil
						return
					}
				}
			}
			errs <- fmt.Errorf("no durable deadline terminal seen: %w", err)
		}()
		waitFor(t, func() bool { return executor.StartCount() == 1 })

		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		var count int
		if err := db.QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM runtime_event WHERE turn_id = 'turn-deadline' AND kind = ?`,
			EventKindTurnFailed).Scan(&count); err != nil {
			t.Fatalf("count terminal: %v", err)
		}
		if count != 1 {
			t.Errorf("durable deadline terminals = %d, want 1", count)
		}
		close(gate)
	})
}

func TestRunCancellationIsDurable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := make(chan struct{})
		executor := NewFakeExecutor([]FakeStep{{Kind: EventKindModelStarted, Block: gate}})
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, executor)
		mustCreateSession(t, db, "session-a")

		ctx, cancel := context.WithCancel(context.Background())
		events := make(chan store.RuntimeEvent, 16)
		errs := make(chan error, 1)
		go func() {
			for ev, err := range engine.Run(ctx, sampleRequest("session-a", "turn-cancel")) {
				if err != nil {
					errs <- err
					return
				}
				events <- ev
			}
			close(events)
			errs <- nil
		}()
		waitFor(t, func() bool { return executor.StartCount() == 1 })

		cancel()
		waitForTerminal(t, events)
		if err := <-errs; err != nil {
			t.Fatalf("cancelled run: %v", err)
		}
		var count int
		if err := db.QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM runtime_event WHERE turn_id = 'turn-cancel' AND kind = ?`,
			EventKindTurnCancelled).Scan(&count); err != nil {
			t.Fatalf("count terminal: %v", err)
		}
		if count != 1 {
			t.Errorf("durable cancel terminals = %d, want 1", count)
		}
		close(gate)
	})
}

func waitForTerminal(t *testing.T, events chan store.RuntimeEvent) {
	t.Helper()
	for ev := range events {
		if isTerminalKind(ev.Kind) {
			return
		}
	}
	t.Fatal("stream ended without a terminal event")
}

func TestShutdownDrainsAndCancelsDurably(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := make(chan struct{})
		executor := NewFakeExecutor([]FakeStep{{Kind: EventKindModelStarted, Block: gate}})
		engine, db, _ := newTestRuntime(t, Config{
			MaxActiveTurns: 1, MaxPendingTurns: 4, ShutdownTimeout: 2 * time.Second,
		}, executor)
		mustCreateSession(t, db, "session-a")

		// One active turn blocked on the gate, two more queued behind it.
		errs := make(chan error, 3)
		for i := range 3 {
			go func(n int) {
				_, err := collect(t, engine, sampleRequest("session-a", turnID(n)))
				errs <- err
			}(i)
		}
		waitFor(t, func() bool { return acceptedCount(t, db, "session-a") >= 3 })

		if err := engine.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}

		// Every accepted turn must have a durable terminal event.
		var accepted, terminals int
		if err := db.QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM runtime_event WHERE session_id = 'session-a' AND kind = ?`,
			EventKindTurnAccepted).Scan(&accepted); err != nil {
			t.Fatalf("count accepted: %v", err)
		}
		if err := db.QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM runtime_event WHERE session_id = 'session-a' AND kind IN (?, ?, ?)`,
			EventKindTurnCompleted, EventKindTurnFailed, EventKindTurnCancelled).Scan(&terminals); err != nil {
			t.Fatalf("count terminals: %v", err)
		}
		if accepted != 3 || terminals != 3 {
			t.Errorf("accepted=%d terminals=%d, want 3 and 3", accepted, terminals)
		}
		close(gate)
		for range 3 {
			<-errs
		}
	})
}

func TestAbandonedConsumerDoesNotBlockTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := make(chan struct{})
		executor := NewFakeExecutor([]FakeStep{{Kind: EventKindModelStarted, Block: gate}})
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, executor)
		mustCreateSession(t, db, "session-a")

		// The consumer reads the accepted event, then abandons the iterator.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		seq := engine.Run(ctx, sampleRequest("session-a", "turn-abandon"))
		next, stop := iter.Pull2(seq)
		first, _, ok := next()
		if !ok || first.Kind != EventKindTurnAccepted {
			t.Fatalf("first event = %v, present=%v", first, ok)
		}
		stop()

		// The turn must still run to a durable completion even though its only
		// subscriber is gone.
		waitFor(t, func() bool { return executor.StartCount() == 1 })
		close(gate)
		waitForTerminalDurable(t, db, "turn-abandon")
	})
}

func TestPublishDoesNotBlockControlPathsWhenSubscriberIsFull(t *testing.T) {
	sub := newSubscriber()
	defer sub.stop()
	for range cap(sub.events) {
		sub.events <- store.RuntimeEvent{}
	}

	cancelled := make(chan struct{})
	engine := &Engine{
		turns: map[string]*turn{
			"turn-live": {
				req:    TurnRequest{SessionID: "session-a"},
				cancel: func() { close(cancelled) },
				subs:   map[*subscriber]struct{}{sub: {}},
			},
		},
	}

	published := make(chan struct{})
	go func() {
		engine.Publish(&store.RuntimeEvent{SessionID: "session-a", TurnID: "turn-live"})
		close(published)
	}()
	cancelDone := make(chan struct{})
	go func() {
		engine.cancelTurn("turn-live")
		close(cancelDone)
	}()

	select {
	case <-published:
	case <-time.After(time.Second):
		t.Fatal("live event publish blocked on a full subscriber")
	}
	select {
	case <-cancelDone:
	case <-time.After(time.Second):
		t.Fatal("control path blocked behind live event publish")
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("cancel path did not reach the live turn")
	}
}

func waitForTerminalDurable(t *testing.T, db *sql.DB, turnID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		if err := db.QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM runtime_event WHERE turn_id = ? AND kind IN (?, ?, ?)`,
			turnID, EventKindTurnCompleted, EventKindTurnFailed, EventKindTurnCancelled).Scan(&count); err != nil {
			t.Fatalf("count terminal: %v", err)
		}
		if count >= 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("turn never reached a durable terminal")
}

func TestSubmitDuringShutdownGetsDurableTerminal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := make(chan struct{})
		executor := NewFakeExecutor([]FakeStep{{Kind: EventKindModelStarted, Block: gate}})
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 1, MaxPendingTurns: 64}, executor)
		mustCreateSession(t, db, "session-a")

		// Occupy the active slot so submits queue as pending.
		go func() {
			_, _ = collect(t, engine, sampleRequest("session-a", "turn-0"))
		}()
		waitFor(t, func() bool { return executor.StartCount() == 1 })

		// Burst submits racing shutdown: each must either be rejected cleanly or
		// reach a durable terminal, and no Run stream may hang.
		const n = 8
		errs := make(chan error, n)
		for i := range n {
			go func(id string) {
				events, err := collect(t, engine, sampleRequest("session-a", id))
				if err != nil {
					errs <- err
					return
				}
				if len(events) == 0 || !isTerminalKind(events[len(events)-1].Kind) {
					errs <- fmt.Errorf("%s: stream ended without a terminal", id)
					return
				}
				errs <- nil
			}("turn-" + string(rune('a'+i)))
		}

		shutdownErr := make(chan error, 1)
		go func() {
			shutdownErr <- engine.Shutdown(context.Background())
		}()
		close(gate)
		for i := range n {
			// Rejection with a stable code is fine; hangs are the failure mode.
			if err := <-errs; err != nil {
				if code, ok := CodeOf(err); !ok || code != ErrorCodeRuntimeOverloaded {
					t.Fatalf("submit %d: %v", i, err)
				}
			}
		}
		if err := <-shutdownErr; err != nil {
			t.Fatalf("Shutdown: %v", err)
		}

		// Invariant: every accepted turn has a durable terminal event.
		var orphans int
		if err := db.QueryRowContext(context.Background(), `
			SELECT COUNT(*) FROM runtime_event a
			WHERE a.kind = ? AND NOT EXISTS (
				SELECT 1 FROM runtime_event b
				WHERE b.turn_id = a.turn_id AND b.kind IN (?, ?, ?)
			)`, EventKindTurnAccepted, EventKindTurnCompleted, EventKindTurnFailed, EventKindTurnCancelled).Scan(&orphans); err != nil {
			t.Fatalf("count orphaned accepts: %v", err)
		}
		if orphans != 0 {
			t.Errorf("accepted turns without a terminal = %d, want 0", orphans)
		}
	})
}

func TestCompletedTurnsArePruned(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := NewFakeExecutor([]FakeStep{{Kind: EventKindModelStarted}})
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, executor)
		mustCreateSession(t, db, "session-a")

		for i := range 5 {
			if _, err := collect(t, engine, sampleRequest("session-a", turnID(i))); err != nil {
				t.Fatalf("turn %d: %v", i, err)
			}
		}

		// The terminal event is broadcast before the worker's deferred prune
		// runs, so wait for the prune rather than asserting synchronously.
		waitFor(t, func() bool {
			engine.mu.Lock()
			defer engine.mu.Unlock()
			return len(engine.turns) == 0
		})

		// Replays of completed turns still work through the store.
		events, err := engine.dedupe.ListTurnEvents(context.Background(), turnID(0))
		if err != nil {
			t.Fatalf("ListTurnEvents after prune: %v", err)
		}
		if len(events) == 0 || !isTerminalKind(events[len(events)-1].Kind) {
			t.Errorf("replayed events = %d, last kind %q; want a full terminal sequence", len(events), lastKind(events))
		}
	})
}

func lastKind(events []store.RuntimeEvent) string {
	if len(events) == 0 {
		return ""
	}
	return events[len(events)-1].Kind
}

// ignoreCancelExecutor blocks until release closes, ignoring ctx
// cancellation, so only the shutdown grace bound can end the turn.
type ignoreCancelExecutor struct {
	release chan struct{}
}

func (e ignoreCancelExecutor) Execute(ctx context.Context, req *TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
	return func(yield func(store.RuntimeEvent, error) bool) {
		<-e.release
	}
}

func TestShutdownBoundedWhenExecutorIgnoresCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := ignoreCancelExecutor{release: make(chan struct{})}
		engine, db, _ := newTestRuntime(t, Config{
			MaxActiveTurns: 1, MaxPendingTurns: 4, ShutdownTimeout: 100 * time.Millisecond,
		}, executor)
		mustCreateSession(t, db, "session-a")

		go func() {
			_, _ = collect(t, engine, sampleRequest("session-a", "turn-0"))
		}()
		waitFor(t, func() bool { return executorStarted(engine) })

		start := time.Now()
		err := engine.Shutdown(context.Background())
		if err == nil {
			t.Fatal("Shutdown returned nil, want runtime_internal after grace")
		}
		if code, ok := CodeOf(err); !ok || code != ErrorCodeRuntimeInternal {
			t.Fatalf("CodeOf(%v) = %q, %v; want runtime_internal", err, code, ok)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("Shutdown took %v, want bounded by the grace period", elapsed)
		}
		close(executor.release)
	})
}

func executorStarted(engine *Engine) bool {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	return engine.active > 0
}
