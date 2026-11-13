package runtimeengine

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

	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/platform/turn/ingress"
	"github.com/anggasct/aura/internal/platform/turn/turntest"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/store/storetest"
)

func newTestRuntime(t *testing.T, cfg Config, executor TurnExecutor) (*Engine, *sql.DB, store.EventStore) {
	t.Helper()
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "aura.db")
	storetest.Seed(t, dsn)
	db, err := store.OpenDB(ctx, dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	events := store.NewEventStore(db)
	dedupe := store.NewDedupeStore(db)
	engine, err := NewEngine(&cfg, events, dedupe, executor, nil)
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

func collect(t *testing.T, engine *Engine, req *turn.TurnRequest) ([]store.RuntimeEvent, error) {
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

func sampleRequest(sessionID, turnID string) *turn.TurnRequest {
	return &turn.TurnRequest{
		TurnID:      turnID,
		SessionID:   sessionID,
		PrincipalID: "user-1",
		Origin:      turn.OriginTerminal,
		Parts:       []runtimeingress.InputPart{{Text: "hello"}},
	}
}

func TestPerSessionFIFONoOverlap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := make(chan struct{})
		executor := turntest.NewFakeExecutor([]turntest.FakeStep{
			{Kind: turn.EventKindModelStarted, Block: gate},
		})
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
		mustCreateSession(t, db, "session-a")

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
		executor := turntest.NewFakeExecutor([]turntest.FakeStep{
			{Kind: turn.EventKindModelStarted, Block: gate},
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
		executor := turntest.NewFakeExecutor([]turntest.FakeStep{
			{Kind: turn.EventKindModelStarted},
			{Kind: turn.EventKindMessageCompleted},
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
		sessionID, turn.EventKindTurnAccepted).Scan(&count); err != nil {
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
		executor := turntest.NewFakeExecutor([]turntest.FakeStep{{Kind: turn.EventKindModelStarted, Block: gate}})
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 1, MaxPendingTurns: 1}, executor)
		mustCreateSession(t, db, "session-a")
		mustCreateSession(t, db, "session-b")
		mustCreateSession(t, db, "session-c")

		errs := make(chan error, 2)
		go func() {
			_, err := collect(t, engine, sampleRequest("session-a", turnID(0)))
			errs <- err
		}()
		waitFor(t, func() bool { return acceptedCount(t, db, "session-a") >= 1 && executor.StartCount() == 1 })
		go func() {
			_, err := collect(t, engine, sampleRequest("session-b", turnID(1)))
			errs <- err
		}()
		waitFor(t, func() bool { return acceptedCount(t, db, "session-b") >= 1 })

		_, err := collect(t, engine, sampleRequest("session-c", turnID(2)))
		code, ok := turn.CodeOf(err)
		if !ok || code != turn.ErrorCodeRuntimeOverloaded {
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
		executor := turntest.NewFakeExecutor([]turntest.FakeStep{{Kind: turn.EventKindModelStarted, Block: gate}})
		engine, db, _ := newTestRuntime(t, Config{
			MaxActiveTurns: 2, MaxPendingTurns: 4, TurnTimeout: 50 * time.Millisecond,
		}, executor)
		mustCreateSession(t, db, "session-a")

		errs := make(chan error, 1)
		go func() {
			events, err := collect(t, engine, sampleRequest("session-a", "turn-deadline"))
			for _, ev := range events {
				if ev.Kind == turn.EventKindTurnFailed {
					var payload struct {
						Code turn.ErrorCode `json:"code"`
					}
					if err := json.Unmarshal(ev.Payload, &payload); err == nil && payload.Code == turn.ErrorCodeTurnDeadlineExceeded {
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
			turn.EventKindTurnFailed).Scan(&count); err != nil {
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
		executor := turntest.NewFakeExecutor([]turntest.FakeStep{{Kind: turn.EventKindModelStarted, Block: gate}})
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
			turn.EventKindTurnCancelled).Scan(&count); err != nil {
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
		executor := turntest.NewFakeExecutor([]turntest.FakeStep{{Kind: turn.EventKindModelStarted, Block: gate}})
		engine, db, _ := newTestRuntime(t, Config{
			MaxActiveTurns: 1, MaxPendingTurns: 4, ShutdownTimeout: 2 * time.Second,
		}, executor)
		mustCreateSession(t, db, "session-a")

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

		var accepted, terminals int
		if err := db.QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM runtime_event WHERE session_id = 'session-a' AND kind = ?`,
			turn.EventKindTurnAccepted).Scan(&accepted); err != nil {
			t.Fatalf("count accepted: %v", err)
		}
		if err := db.QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM runtime_event WHERE session_id = 'session-a' AND kind IN (?, ?, ?)`,
			turn.EventKindTurnCompleted, turn.EventKindTurnFailed, turn.EventKindTurnCancelled).Scan(&terminals); err != nil {
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
		executor := turntest.NewFakeExecutor([]turntest.FakeStep{{Kind: turn.EventKindModelStarted, Block: gate}})
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, executor)
		mustCreateSession(t, db, "session-a")

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		seq := engine.Run(ctx, sampleRequest("session-a", "turn-abandon"))
		next, stop := iter.Pull2(seq)
		first, _, ok := next()
		if !ok || first.Kind != turn.EventKindTurnAccepted {
			t.Fatalf("first event = %v, present=%v", first, ok)
		}
		stop()

		waitFor(t, func() bool { return executor.StartCount() == 1 })
		close(gate)
		waitForTerminalDurable(t, db, "turn-abandon")
	})
}

func TestPublishDoesNotBlockControlPathsWhenSubscriberIsFull(t *testing.T) {
	sub := newSubscriber()
	defer sub.stop()
	for range cap(sub.events) {
		sub.events <- store.RuntimeEvent{Kind: turn.EventKindModelDelta}
	}

	cancelled := make(chan struct{})
	bus := turn.NewEventBus(nil)
	engine, _, _ := newTestRuntime(t, Config{LiveEvents: bus}, turntest.NewFakeExecutor(nil))
	engine.turns["turn-live"] = &turnState{
		req:    turn.TurnRequest{SessionID: "session-a"},
		cancel: func() { close(cancelled) },
		subs:   map[*subscriber]struct{}{sub: {}},
	}

	published := make(chan struct{})
	go func() {
		bus.Publish(&store.RuntimeEvent{SessionID: "session-a", TurnID: "turn-live", Kind: "tool.requested"})
		close(published)
	}()
	cancelDone := make(chan struct{})
	go func() {
		engine.cancelTurn("turn-live")
		close(cancelDone)
	}()

	select {
	case <-cancelDone:
	case <-time.After(time.Second):
		t.Fatal("control path blocked behind live event publish")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("cancel path did not reach the live turn")
	}
	select {
	case <-published:
	case <-time.After(time.Second):
		t.Fatal("live event publisher blocked on coalescible deltas")
	}

	found := false
	for range cap(sub.events) + 1 {
		select {
		case ev := <-sub.events:
			if ev.Kind == "tool.requested" {
				found = true
			}
		case <-time.After(time.Second):
			t.Fatal("published lifecycle event was not delivered")
		}
		if found {
			break
		}
	}
	if !found {
		t.Fatal("published lifecycle event was not delivered")
	}
}

func TestEngineRelaysBusEventsToItsRunningTurn(t *testing.T) {
	gate := make(chan struct{})
	executor := turntest.NewFakeExecutor([]turntest.FakeStep{{Kind: turn.EventKindModelStarted, Block: gate}})
	bus := turn.NewEventBus(nil)
	engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 1, MaxPendingTurns: 2, LiveEvents: bus}, executor)
	mustCreateSession(t, db, "session-a")

	next, stop := iter.Pull2(engine.Run(t.Context(), sampleRequest("session-a", "turn-relay")))
	defer stop()
	first, err, ok := next()
	if err != nil || !ok || first.Kind != turn.EventKindTurnAccepted {
		t.Fatalf("first event = %+v, err=%v, present=%v; want accepted", first, err, ok)
	}
	waitFor(t, func() bool { return executor.StartCount() == 1 })

	bus.Publish(&store.RuntimeEvent{ID: "foreign", SessionID: "session-b", TurnID: "turn-relay", Kind: turn.EventKindToolRequested})
	bus.Publish(&store.RuntimeEvent{ID: "relayed", SessionID: "session-a", TurnID: "turn-relay", Kind: turn.EventKindToolRequested})
	close(gate)

	var relayed, foreign int
	for {
		ev, err, ok := next()
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		if !ok {
			break
		}
		switch ev.ID {
		case "relayed":
			relayed++
		case "foreign":
			foreign++
		}
		if ev.Kind == turn.EventKindTurnCompleted || ev.Kind == turn.EventKindTurnFailed {
			break
		}
	}
	if relayed != 1 {
		t.Fatalf("relayed events = %d, want 1", relayed)
	}
	if foreign != 0 {
		t.Fatalf("events for another session = %d, want 0", foreign)
	}
}

func TestPublishAfterSubscriberCloseDoesNotPanic(t *testing.T) {
	sub := newSubscriber()
	sub.closeEvents()
	bus := turn.NewEventBus(nil)
	engine, _, _ := newTestRuntime(t, Config{LiveEvents: bus}, turntest.NewFakeExecutor(nil))
	engine.turns["turn-live"] = &turnState{
		req:  turn.TurnRequest{SessionID: "session-a"},
		subs: map[*subscriber]struct{}{sub: {}},
	}
	done := make(chan struct{})
	go func() {
		bus.Publish(&store.RuntimeEvent{SessionID: "session-a", TurnID: "turn-live", Kind: "tool.requested"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("publish blocked on closed subscriber")
	}
}

func TestSaturatedLifecycleSubscriberReplaysAfterGap(t *testing.T) {
	script := make([]turntest.FakeStep, subscriberBufferSize+1)
	for i := range script {
		script[i] = turntest.FakeStep{Kind: turn.EventKindToolRequested, Payload: []byte(`{"tool":"test"}`)}
	}
	engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 1, MaxPendingTurns: 2}, turntest.NewFakeExecutor(script))
	mustCreateSession(t, db, "session-a")

	next, stop := iter.Pull2(engine.Run(context.Background(), sampleRequest("session-a", "turn-gap")))
	defer stop()
	first, err, ok := next()
	if err != nil || !ok || first.Kind != turn.EventKindTurnAccepted {
		t.Fatalf("first event = %+v, err=%v, present=%v; want accepted", first, err, ok)
	}

	var requested int
	for {
		ev, err, ok := next()
		if err != nil {
			t.Fatalf("next event: %v", err)
		}
		if !ok {
			t.Fatal("stream closed before terminal replay")
		}
		if ev.Kind == turn.EventKindToolRequested {
			requested++
		}
		if isTerminalKind(ev.Kind) {
			if requested != len(script) {
				t.Fatalf("replayed lifecycle events = %d, want %d", requested, len(script))
			}
			return
		}
	}
}

func acceptedSessionOf(t *testing.T, db *sql.DB, turnID string) string {
	t.Helper()
	var sessionID string
	err := db.QueryRowContext(context.Background(), `SELECT session_id FROM runtime_event WHERE turn_id = ? AND kind = ?`, turnID, turn.EventKindTurnAccepted).Scan(&sessionID)
	if err != nil {
		t.Fatalf("read the session of %s: %v", turnID, err)
	}
	return sessionID
}

func waitForTerminalDurable(t *testing.T, db *sql.DB, turnID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		if err := db.QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM runtime_event WHERE turn_id = ? AND kind IN (?, ?, ?)`,
			turnID, turn.EventKindTurnCompleted, turn.EventKindTurnFailed, turn.EventKindTurnCancelled).Scan(&count); err != nil {
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
		executor := turntest.NewFakeExecutor([]turntest.FakeStep{{Kind: turn.EventKindModelStarted, Block: gate}})
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 1, MaxPendingTurns: 64}, executor)
		mustCreateSession(t, db, "session-a")

		go func() {
			_, _ = collect(t, engine, sampleRequest("session-a", "turn-0"))
		}()
		waitFor(t, func() bool { return executor.StartCount() == 1 })

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
			if err := <-errs; err != nil {
				if code, ok := turn.CodeOf(err); !ok || code != turn.ErrorCodeRuntimeOverloaded {
					t.Fatalf("submit %d: %v", i, err)
				}
			}
		}
		if err := <-shutdownErr; err != nil {
			t.Fatalf("Shutdown: %v", err)
		}

		var orphans int
		if err := db.QueryRowContext(context.Background(), `
			SELECT COUNT(*) FROM runtime_event a
			WHERE a.kind = ? AND NOT EXISTS (
				SELECT 1 FROM runtime_event b
				WHERE b.turn_id = a.turn_id AND b.kind IN (?, ?, ?)
			)`, turn.EventKindTurnAccepted, turn.EventKindTurnCompleted, turn.EventKindTurnFailed, turn.EventKindTurnCancelled).Scan(&orphans); err != nil {
			t.Fatalf("count orphaned accepts: %v", err)
		}
		if orphans != 0 {
			t.Errorf("accepted turns without a terminal = %d, want 0", orphans)
		}
	})
}

func TestCompletedTurnsArePruned(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := turntest.NewFakeExecutor([]turntest.FakeStep{{Kind: turn.EventKindModelStarted}})
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, executor)
		mustCreateSession(t, db, "session-a")

		for i := range 5 {
			if _, err := collect(t, engine, sampleRequest("session-a", turnID(i))); err != nil {
				t.Fatalf("turn %d: %v", i, err)
			}
		}

		waitFor(t, func() bool {
			engine.mu.Lock()
			defer engine.mu.Unlock()
			return len(engine.turns) == 0
		})

		events, err := engine.dedupe.ListTurnEvents(context.Background(), "session-a", turnID(0))
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

type ignoreCancelExecutor struct {
	release chan struct{}
}

func (e ignoreCancelExecutor) Execute(ctx context.Context, req *turn.TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
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
		if code, ok := turn.CodeOf(err); !ok || code != turn.ErrorCodeRuntimeInternal {
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
