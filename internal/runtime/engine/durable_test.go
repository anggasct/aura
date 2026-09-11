package runtimeengine

import (
	"context"
	"database/sql"
	"encoding/json"
	"iter"
	"testing"
	"testing/synctest"
	"time"

	"github.com/anggasct/aura/internal/durable"
	"github.com/anggasct/aura/internal/runtime"
	runtimesessions "github.com/anggasct/aura/internal/runtime/sessions"
	"github.com/anggasct/aura/internal/store"
)

func collectDurableTerminal(t *testing.T, db *sql.DB, turnID string) []store.RuntimeEvent {
	t.Helper()
	waitForTerminalDurable(t, db, turnID)
	events, err := store.NewDedupeStore(db).ListTurnEvents(context.Background(), turnID)
	if err != nil {
		t.Fatalf("list turn events: %v", err)
	}
	return events
}

func TestDurableAdmitCompletesTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := runtime.NewFakeExecutor([]runtime.FakeStep{
			{Kind: runtime.EventKindModelStarted, Payload: []byte(`{}`)},
			{Kind: runtime.EventKindMessageCompleted, Payload: []byte(`{}`)},
		})
		engine, _, db := newDurableTestRuntime(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
		mustCreateSession(t, db, "session-a")

		events, err := collect(t, engine, sampleRequest("session-a", "turn-0"))
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if last := events[len(events)-1]; last.Kind != runtime.EventKindTurnCompleted {
			t.Fatalf("terminal kind = %q, want turn.completed", last.Kind)
		}
		if events[0].Sequence != 1 {
			t.Fatalf("accepted sequence = %d, want 1", events[0].Sequence)
		}
	})
}

func TestDurableSessionFIFO(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := make(chan struct{})
		executor := runtime.NewFakeExecutor([]runtime.FakeStep{
			{Kind: runtime.EventKindModelStarted, Payload: []byte(`{}`), Block: gate},
		})
		engine, _, db := newDurableTestRuntime(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
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

func TestDurableDuplicateKeyReplaysWithoutReexecution(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := runtime.NewFakeExecutor([]runtime.FakeStep{
			{Kind: runtime.EventKindModelStarted, Payload: []byte(`{}`)},
		})
		engine, _, db := newDurableTestRuntime(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
		mustCreateSession(t, db, "session-a")

		first := sampleRequest("session-a", "turn-0")
		first.IdempotencyKey = "ext-1"
		second := sampleRequest("session-a", "turn-1")
		second.IdempotencyKey = "ext-1"
		firstEvents, err := collect(t, engine, first)
		if err != nil {
			t.Fatalf("first run: %v", err)
		}
		secondEvents, err := collect(t, engine, second)
		if err != nil {
			t.Fatalf("second run: %v", err)
		}
		if got := executor.StartCount(); got != 1 {
			t.Fatalf("StartCount = %d, want 1 (duplicate must not re-execute)", got)
		}
		if len(firstEvents) != len(secondEvents) {
			t.Fatalf("replay length = %d, want %d", len(secondEvents), len(firstEvents))
		}
		for i := range firstEvents {
			if firstEvents[i].Kind != secondEvents[i].Kind || firstEvents[i].TurnID != secondEvents[i].TurnID {
				t.Fatalf("replay event %d diverges: %+v vs %+v", i, secondEvents[i], firstEvents[i])
			}
		}
	})
}

func TestDurableQueueOverflowFailsClosed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := make(chan struct{})
		defer close(gate)
		executor := runtime.NewFakeExecutor([]runtime.FakeStep{
			{Kind: runtime.EventKindModelStarted, Payload: []byte(`{}`), Block: gate},
		})
		engine, _, db := newDurableTestRuntime(t, Config{MaxActiveTurns: 1, MaxPendingTurns: 1}, executor)
		mustCreateSession(t, db, "session-a")

		go func() { _, _ = collect(t, engine, sampleRequest("session-a", "turn-0")) }()
		waitFor(t, func() bool { return executor.StartCount() == 1 })
		_, err := collect(t, engine, sampleRequest("session-a", "turn-1"))
		if err == nil {
			t.Fatal("expected an overload error")
		}
		code, ok := runtime.CodeOf(err)
		if !ok || code != runtime.ErrorCodeRuntimeOverloaded {
			t.Fatalf("code = %v, want runtime_overloaded", err)
		}
		if got := acceptedCount(t, db, "session-a"); got != 1 {
			t.Fatalf("accepted events = %d, want 1 (overloaded admit must persist nothing)", got)
		}
		rejected, err := store.NewDedupeStore(db).ListTurnEvents(context.Background(), "turn-1")
		if err != nil {
			t.Fatalf("list rejected turn events: %v", err)
		}
		if len(rejected) != 0 {
			t.Fatalf("rejected turn events = %d, want 0 (no orphan open turn)", len(rejected))
		}
	})
}

func transcript(events []store.RuntimeEvent) [][2]string {
	out := make([][2]string, 0, len(events))
	for i := range events {
		out = append(out, [2]string{events[i].Kind, string(events[i].Payload)})
	}
	return out
}

func TestDurableParityWithLegacy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		script := []runtime.FakeStep{
			{Kind: runtime.EventKindModelStarted, Payload: []byte(`{}`)},
			{Kind: runtime.EventKindModelDelta, Payload: []byte(`{}`)},
			{Kind: runtime.EventKindMessageCompleted, Payload: []byte(`{}`)},
		}
		requests := func() []*runtime.TurnRequest {
			return []*runtime.TurnRequest{
				sampleRequest("session-a", "turn-0"),
				sampleRequest("session-a", "turn-1"),
				sampleRequest("session-b", "turn-2"),
			}
		}
		runAll := func(engine *Engine, db *sql.DB) [][][2]string {
			var transcripts [][][2]string
			for _, req := range requests() {
				events, err := collect(t, engine, req)
				if err != nil {
					t.Fatalf("run %s: %v", req.TurnID, err)
				}
				transcripts = append(transcripts, transcript(events))
			}
			return transcripts
		}
		legacyExecutor := runtime.NewFakeExecutor(script)
		legacy, legacyDB, _ := newTestRuntime(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, legacyExecutor)
		mustCreateSession(t, legacyDB, "session-a")
		mustCreateSession(t, legacyDB, "session-b")
		legacyTranscripts := runAll(legacy, legacyDB)

		durableExecutor := runtime.NewFakeExecutor(script)
		durableEngine, _, durableDB := newDurableTestRuntime(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, durableExecutor)
		mustCreateSession(t, durableDB, "session-a")
		mustCreateSession(t, durableDB, "session-b")
		durableTranscripts := runAll(durableEngine, durableDB)

		if len(legacyTranscripts) != len(durableTranscripts) {
			t.Fatalf("transcript count = %d, want %d", len(durableTranscripts), len(legacyTranscripts))
		}
		for i := range legacyTranscripts {
			if len(legacyTranscripts[i]) != len(durableTranscripts[i]) {
				t.Fatalf("turn %d length = %d, want %d", i, len(durableTranscripts[i]), len(legacyTranscripts[i]))
			}
			for j := range legacyTranscripts[i] {
				if legacyTranscripts[i][j] != durableTranscripts[i][j] {
					t.Fatalf("turn %d event %d diverges:\nlegacy:  %+v\ndurable: %+v", i, j, legacyTranscripts[i][j], durableTranscripts[i][j])
				}
			}
		}
	})
}

func TestDurableShutdownCancelsStaged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := make(chan struct{})
		executor := runtime.NewFakeExecutor([]runtime.FakeStep{
			{Kind: runtime.EventKindModelStarted, Payload: []byte(`{}`), Block: gate},
		})
		engine, _, db := newDurableTestRuntime(t, Config{MaxActiveTurns: 1, MaxPendingTurns: 16, ShutdownTimeout: 5 * time.Second}, executor)
		mustCreateSession(t, db, "session-a")

		go func() { _, _ = collect(t, engine, sampleRequest("session-a", "turn-0")) }()
		waitFor(t, func() bool { return executor.StartCount() == 1 })
		go func() { _, _ = collect(t, engine, sampleRequest("session-a", "turn-1")) }()
		waitFor(t, func() bool { return acceptedCount(t, db, "session-a") == 2 })

		if err := engine.Shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
		close(gate)
		events := collectDurableTerminal(t, db, "turn-1")
		if last := events[len(events)-1]; last.Kind != runtime.EventKindTurnCancelled {
			t.Fatalf("staged turn terminal = %q, want turn.cancelled", last.Kind)
		}
	})
}

func TestDescriptorRoundTripPreservesDeadline(t *testing.T) {
	deadline := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	req := sampleRequest("session-a", "turn-0")
	req.Deadline = deadline
	req.Budget.MaxTokens = 1000
	desc := turnDescriptorFromRequest(req)
	roundTripped := turnRequestFromDescriptor(&desc)
	if !roundTripped.Deadline.Equal(deadline) {
		t.Fatalf("deadline = %v, want %v", roundTripped.Deadline, deadline)
	}
	if roundTripped.Budget.MaxTokens != 1000 || roundTripped.TurnID != "turn-0" || string(roundTripped.Origin) != "terminal" {
		t.Fatalf("round trip lost fields: %+v", roundTripped)
	}
}

func TestDurableLongStreamReplaysFullyFromStore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const deltas = 100
		script := make([]runtime.FakeStep, 0, deltas+1)
		for i := range deltas {
			script = append(script, runtime.FakeStep{
				Kind:    runtime.EventKindModelDelta,
				Payload: []byte(`{"n":` + string(rune('0'+i%10)) + `}`),
			})
		}
		script = append(script, runtime.FakeStep{Kind: runtime.EventKindMessageCompleted, Payload: []byte(`{}`)})
		executor := runtime.NewFakeExecutor(script)
		engine, _, db := newDurableTestRuntime(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
		mustCreateSession(t, db, "session-a")

		events, err := collect(t, engine, sampleRequest("session-a", "turn-0"))
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if len(events) != deltas+3 {
			t.Fatalf("streamed events = %d, want %d", len(events), deltas+3)
		}
		for i := 1; i < len(events); i++ {
			if events[i].Sequence != events[i-1].Sequence+1 {
				t.Fatalf("stream sequences not contiguous at %d: %d after %d", i, events[i].Sequence, events[i-1].Sequence)
			}
		}
		stored, err := store.NewDedupeStore(db).ListTurnEvents(context.Background(), "turn-0")
		if err != nil {
			t.Fatalf("list turn events: %v", err)
		}
		if len(stored) != len(events) {
			t.Fatalf("stored events = %d, want %d", len(stored), len(events))
		}
		for i := range stored {
			if stored[i].Kind != events[i].Kind || string(stored[i].Payload) != string(events[i].Payload) {
				t.Fatalf("stored event %d diverges from the stream", i)
			}
		}
	})
}

func TestDurableLiveStreamDeliversPersistedSequence(t *testing.T) {
	script := []runtime.FakeStep{
		{Kind: runtime.EventKindModelStarted, Payload: []byte(`{"s":1}`)},
		{Kind: runtime.EventKindModelDelta, Payload: []byte(`{"d":1}`)},
		{Kind: runtime.EventKindModelDelta, Payload: []byte(`{"d":2}`)},
		{Kind: runtime.EventKindMessageCompleted, Payload: []byte(`{}`)},
	}
	executor := runtime.NewFakeExecutor(script)
	engine, _, db := newDurableLiveTestRuntime(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
	mustCreateSession(t, db, "session-live")

	streamed, err := collect(t, engine, sampleRequest("session-live", "turn-live-0"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	stored, err := store.NewDedupeStore(db).ListTurnEvents(context.Background(), "turn-live-0")
	if err != nil {
		t.Fatalf("list turn events: %v", err)
	}
	if len(streamed) != len(stored) {
		t.Fatalf("streamed events = %d, stored = %d, want equal", len(streamed), len(stored))
	}
	for i := range stored {
		if streamed[i].Kind != stored[i].Kind || streamed[i].Sequence != stored[i].Sequence {
			t.Fatalf("event %d diverges: stream %+v vs stored %+v", i, streamed[i], stored[i])
		}
		if i > 0 && streamed[i].Sequence != streamed[i-1].Sequence+1 {
			t.Fatalf("stream sequences not contiguous at %d", i)
		}
	}
	if last := streamed[len(streamed)-1]; !isTerminalKind(last.Kind) {
		t.Fatalf("terminal kind = %q, want completed/failed/cancelled", last.Kind)
	}
}

func TestDurableChainCompletesFIFO(t *testing.T) {
	gate := make(chan struct{})
	executor := runtime.NewFakeExecutor([]runtime.FakeStep{
		{Kind: runtime.EventKindModelStarted, Payload: []byte(`{}`), Block: gate},
	})
	engine, _, db := newDurableLiveTestRuntime(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
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
	for i := range 3 {
		events := collectDurableTerminal(t, db, turnID(i))
		if last := events[len(events)-1]; last.Kind != runtime.EventKindTurnCompleted {
			t.Fatalf("turn %d terminal = %q, want turn.completed", i, last.Kind)
		}
	}
}

func TestDurableResumeNeedsNoScan(t *testing.T) {
	executor := runtime.NewFakeExecutor([]runtime.FakeStep{
		{Kind: runtime.EventKindModelStarted, Payload: []byte(`{}`)},
	})
	_, db, events := newTestRuntime(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
	mustCreateSession(t, db, "session-a")
	stub := newStubSessionStore(events, store.NewDedupeStore(db))
	backend := durable.NewFake()
	ctx := context.Background()
	for _, turn := range []string{"turn-0", "turn-1"} {
		desc := turnDescriptorFromRequest(sampleRequest("session-a", turn))
		if _, err := stub.Admit(ctx, &runtimesessions.AdmitRequest{
			Turn:            desc,
			AcceptedEventID: "event-" + turn,
			MaxPending:      16,
		}); err != nil {
			t.Fatalf("preload admit %s: %v", turn, err)
		}
	}
	restarted, err := NewEngine(Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, events, store.NewDedupeStore(db), executor, nil)
	if err != nil {
		t.Fatalf("restarted engine: %v", err)
	}
	restarted.sessionStore = stub
	restarted.durableRuntime = backend
	backend.RegisterHandler("turn", restarted.serveTurn)
	desc := turnDescriptorFromRequest(sampleRequest("session-a", "turn-0"))
	payload, err := json.Marshal(desc)
	if err != nil {
		t.Fatalf("marshal descriptor: %v", err)
	}
	if _, err := backend.Start(ctx, durable.StartRequest{Handler: "turn", Key: "turn-0", Payload: payload}); err != nil {
		t.Fatalf("restart run turn-0: %v", err)
	}
	for _, turn := range []string{"turn-0", "turn-1"} {
		events := collectDurableTerminal(t, db, turn)
		if last := events[len(events)-1]; last.Kind != runtime.EventKindTurnCompleted {
			t.Fatalf("%s terminal = %q, want turn.completed", turn, last.Kind)
		}
	}
	if got := executor.StartCount(); got != 2 {
		t.Fatalf("StartCount = %d, want 2 (queued turn resumes without a boot scan)", got)
	}
	if got := executor.StartOrder(); got[0] != "turn-0" || got[1] != "turn-1" {
		t.Fatalf("StartOrder = %v, want admission order", got)
	}
}

func TestDurableDeadlineIsTerminal(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)
	executor := runtime.NewFakeExecutor([]runtime.FakeStep{{Kind: runtime.EventKindModelStarted, Payload: []byte(`{}`), Block: gate}})
	engine, _, db := newDurableLiveTestRuntime(t, Config{
		MaxActiveTurns: 2, MaxPendingTurns: 4, TurnTimeout: 200 * time.Millisecond,
	}, executor)
	mustCreateSession(t, db, "session-a")

	events, err := collect(t, engine, sampleRequest("session-a", "turn-deadline"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	last := events[len(events)-1]
	if last.Kind != runtime.EventKindTurnFailed {
		t.Fatalf("terminal kind = %q, want turn.failed", last.Kind)
	}
	var payload struct {
		Code runtime.ErrorCode `json:"code"`
	}
	if err := json.Unmarshal(last.Payload, &payload); err != nil {
		t.Fatalf("decode terminal payload: %v", err)
	}
	if payload.Code != runtime.ErrorCodeTurnDeadlineExceeded {
		t.Fatalf("terminal code = %q, want turn_deadline_exceeded", payload.Code)
	}
	var count int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM runtime_event WHERE turn_id = 'turn-deadline' AND kind = ?`,
		runtime.EventKindTurnFailed).Scan(&count); err != nil {
		t.Fatalf("count terminal: %v", err)
	}
	if count != 1 {
		t.Fatalf("durable deadline terminals = %d, want 1", count)
	}
}

type errorExecutor struct {
	err error
}

func (e errorExecutor) Execute(_ context.Context, _ *runtime.TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
	return func(yield func(store.RuntimeEvent, error) bool) {
		yield(store.RuntimeEvent{}, e.err)
	}
}

func TestDurableBudgetExhaustedIsTerminal(t *testing.T) {
	executor := errorExecutor{err: &runtime.Error{Code: runtime.ErrorCodeBudgetExhausted, Detail: "test budget"}}
	engine, _, db := newDurableLiveTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, executor)
	mustCreateSession(t, db, "session-a")

	events, err := collect(t, engine, sampleRequest("session-a", "turn-budget"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	last := events[len(events)-1]
	if last.Kind != runtime.EventKindTurnFailed {
		t.Fatalf("terminal kind = %q, want turn.failed", last.Kind)
	}
	var payload struct {
		Code runtime.ErrorCode `json:"code"`
	}
	if err := json.Unmarshal(last.Payload, &payload); err != nil {
		t.Fatalf("decode terminal payload: %v", err)
	}
	if payload.Code != runtime.ErrorCodeBudgetExhausted {
		t.Fatalf("terminal code = %q, want budget_exhausted", payload.Code)
	}
	var count int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM runtime_event WHERE turn_id = 'turn-budget' AND kind = ?`,
		runtime.EventKindTurnFailed).Scan(&count); err != nil {
		t.Fatalf("count terminal: %v", err)
	}
	if count != 1 {
		t.Fatalf("durable budget terminals = %d, want 1", count)
	}
}

func TestDurableCancelIsTerminal(t *testing.T) {
	gate := make(chan struct{})
	executor := runtime.NewFakeExecutor([]runtime.FakeStep{{Kind: runtime.EventKindModelStarted, Payload: []byte(`{}`), Block: gate}})
	engine, _, db := newDurableLiveTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, executor)
	mustCreateSession(t, db, "session-a")

	ctx, cancel := context.WithCancel(context.Background())
	streamed := make(chan store.RuntimeEvent, 16)
	errs := make(chan error, 1)
	go func() {
		for ev, err := range engine.Run(ctx, sampleRequest("session-a", "turn-cancel")) {
			if err != nil {
				errs <- err
				return
			}
			streamed <- ev
		}
		close(streamed)
		errs <- nil
	}()
	waitFor(t, func() bool { return executor.StartCount() == 1 })
	cancel()
	for ev := range streamed {
		if isTerminalKind(ev.Kind) {
			if ev.Kind != runtime.EventKindTurnCancelled {
				t.Fatalf("terminal kind = %q, want turn.cancelled", ev.Kind)
			}
		}
	}
	if err := <-errs; err != nil {
		t.Fatalf("cancelled run: %v", err)
	}
	close(gate)
	var count int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM runtime_event WHERE turn_id = 'turn-cancel' AND kind = ?`,
		runtime.EventKindTurnCancelled).Scan(&count); err != nil {
		t.Fatalf("count terminal: %v", err)
	}
	if count != 1 {
		t.Fatalf("durable cancel terminals = %d, want 1", count)
	}
	next, err := collect(t, engine, sampleRequest("session-a", "turn-next"))
	if err != nil {
		t.Fatalf("turn after cancel: %v", err)
	}
	if last := next[len(next)-1]; last.Kind != runtime.EventKindTurnCompleted {
		t.Fatalf("next terminal = %q, want turn.completed (cancel must advance the queue)", last.Kind)
	}
}

func TestDurableShutdownDrainsActive(t *testing.T) {
	gate := make(chan struct{})
	executor := runtime.NewFakeExecutor([]runtime.FakeStep{{Kind: runtime.EventKindModelStarted, Payload: []byte(`{}`), Block: gate}})
	engine, _, db := newDurableLiveTestRuntime(t, Config{
		MaxActiveTurns: 1, MaxPendingTurns: 4, ShutdownTimeout: 5 * time.Second,
	}, executor)
	mustCreateSession(t, db, "session-a")

	errs := make(chan error, 1)
	go func() {
		_, err := collect(t, engine, sampleRequest("session-a", "turn-0"))
		errs <- err
	}()
	waitFor(t, func() bool { return executor.StartCount() == 1 })
	shutdownErr := make(chan error, 1)
	go func() { shutdownErr <- engine.Shutdown(context.Background()) }()
	waitFor(t, func() bool {
		engine.mu.Lock()
		defer engine.mu.Unlock()
		return engine.shutdown
	})
	close(gate)
	if err := <-shutdownErr; err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := <-errs; err != nil {
		t.Fatalf("drained turn: %v", err)
	}
	events := collectDurableTerminal(t, db, "turn-0")
	if last := events[len(events)-1]; last.Kind != runtime.EventKindTurnCompleted {
		t.Fatalf("drained terminal = %q, want turn.completed", last.Kind)
	}
}
