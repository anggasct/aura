package runtimeengine

import (
	"context"
	"database/sql"
	"testing"
	"testing/synctest"
	"time"

	"github.com/anggasct/aura/internal/runtime"
	runtimesessions "github.com/anggasct/aura/internal/runtime/sessions"
	"github.com/anggasct/aura/internal/store"
)

func testDescriptorFor(sessionID, turnID, key string) runtimesessions.Descriptor {
	return runtimesessions.Descriptor{
		TurnID:         turnID,
		SessionID:      sessionID,
		PrincipalID:    "user-1",
		Origin:         "terminal",
		Parts:          []runtimesessions.Part{{Text: "hello"}},
		IdempotencyKey: key,
	}
}

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
		durable, _, durableDB := newDurableTestRuntime(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, durableExecutor)
		mustCreateSession(t, durableDB, "session-a")
		mustCreateSession(t, durableDB, "session-b")
		durableTranscripts := runAll(durable, durableDB)

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

func TestDurableRecoverSessionRestarts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := runtime.NewFakeExecutor([]runtime.FakeStep{
			{Kind: runtime.EventKindModelStarted, Payload: []byte(`{}`)},
		})
		engine, stub, db := newDurableTestRuntime(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
		mustCreateSession(t, db, "session-a")

		ctx := context.Background()
		admit, err := stub.Admit(ctx, admitRequestFor("session-a", "turn-0", ""))
		if err != nil {
			t.Fatalf("direct admit: %v", err)
		}
		if admit.ToStart == nil {
			t.Fatal("expected an immediate start grant")
		}
		if err := engine.RecoverSession(ctx, "session-a", []string{"turn-0"}, nil); err != nil {
			t.Fatalf("recover: %v", err)
		}
		engine.mu.Lock()
		pending := engine.pending
		engine.mu.Unlock()
		if pending < 0 {
			t.Fatalf("pending = %d, want non-negative after recover", pending)
		}
		events := collectDurableTerminal(t, db, "turn-0")
		if last := events[len(events)-1]; last.Kind != runtime.EventKindTurnCompleted {
			t.Fatalf("recovered turn terminal = %q, want turn.completed", last.Kind)
		}
		if got := executor.StartCount(); got != 1 {
			t.Fatalf("StartCount = %d, want 1", got)
		}
	})
}

func TestDurableRecoverKeepsPendingNonNegative(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := runtime.NewFakeExecutor([]runtime.FakeStep{
			{Kind: runtime.EventKindModelStarted, Payload: []byte(`{}`)},
		})
		engine, stub, db := newDurableTestRuntime(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
		mustCreateSession(t, db, "session-a")
		mustCreateSession(t, db, "session-b")

		ctx := context.Background()
		if _, err := stub.Admit(ctx, admitRequestFor("session-a", "turn-0", "")); err != nil {
			t.Fatalf("direct admit session-a: %v", err)
		}
		if _, err := stub.Admit(ctx, admitRequestFor("session-b", "turn-1", "")); err != nil {
			t.Fatalf("direct admit session-b: %v", err)
		}
		for _, tc := range []struct{ session, turn string }{
			{"session-a", "turn-0"},
			{"session-b", "turn-1"},
		} {
			if err := engine.RecoverSession(ctx, tc.session, []string{tc.turn}, nil); err != nil {
				t.Fatalf("recover %s: %v", tc.session, err)
			}
			engine.mu.Lock()
			pending := engine.pending
			engine.mu.Unlock()
			if pending < 0 {
				t.Fatalf("pending = %d, want non-negative after recovering %s", pending, tc.session)
			}
		}
		for _, turn := range []string{"turn-0", "turn-1"} {
			events := collectDurableTerminal(t, db, turn)
			if last := events[len(events)-1]; last.Kind != runtime.EventKindTurnCompleted {
				t.Fatalf("recovered %s terminal = %q, want turn.completed", turn, last.Kind)
			}
		}
		engine.mu.Lock()
		pending := engine.pending
		engine.mu.Unlock()
		if pending < 0 {
			t.Fatalf("pending = %d, want non-negative after multi-session recovery", pending)
		}
		if got := executor.StartCount(); got != 2 {
			t.Fatalf("StartCount = %d, want 2", got)
		}
	})
}

func TestDurableRecoverSessionSkipsTerminal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := runtime.NewFakeExecutor([]runtime.FakeStep{
			{Kind: runtime.EventKindModelStarted, Payload: []byte(`{}`)},
		})
		engine, _, db := newDurableTestRuntime(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
		mustCreateSession(t, db, "session-a")

		if _, err := collect(t, engine, sampleRequest("session-a", "turn-0")); err != nil {
			t.Fatalf("run: %v", err)
		}
		if err := engine.RecoverSession(context.Background(), "session-a", nil, []string{"turn-0"}); err != nil {
			t.Fatalf("recover: %v", err)
		}
		if got := executor.StartCount(); got != 1 {
			t.Fatalf("StartCount = %d, want 1 (terminal turn must not restart)", got)
		}
	})
}

func admitRequestFor(sessionID, turnID, key string) *runtimesessions.AdmitRequest {
	return &runtimesessions.AdmitRequest{
		Turn:            testDescriptorFor(sessionID, turnID, key),
		AcceptedEventID: "event-" + turnID,
		MaxPending:      16,
	}
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

func TestDurableAdmitWaitsForRecoveryGate(t *testing.T) {
	executor := runtime.NewFakeExecutor([]runtime.FakeStep{
		{Kind: runtime.EventKindModelStarted, Payload: []byte(`{}`)},
	})
	engine, _, db := newUnrecoveredDurableTestRuntime(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
	mustCreateSession(t, db, "session-a")

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for ev, err := range engine.Run(cancelled, sampleRequest("session-a", "turn-0")) {
		_ = ev
		if err == nil {
			t.Fatal("expected a recovery-pending error")
		}
		if code, ok := runtime.CodeOf(err); !ok || code != runtime.ErrorCodeRuntimeOverloaded {
			t.Fatalf("code = %v, want runtime_overloaded", err)
		}
		break
	}
	engine.MarkRecovered()
	engine.MarkRecovered()
	events, err := collect(t, engine, sampleRequest("session-a", "turn-1"))
	if err != nil {
		t.Fatalf("run after recovery: %v", err)
	}
	if last := events[len(events)-1]; last.Kind != runtime.EventKindTurnCompleted {
		t.Fatalf("terminal = %q, want turn.completed", last.Kind)
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
