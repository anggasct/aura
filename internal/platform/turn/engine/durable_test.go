package runtimeengine

import (
	"context"
	"database/sql"
	"encoding/json"
	"iter"
	"testing"
	"testing/synctest"
	"time"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimesessions "github.com/anggasct/aura/internal/platform/turn/sessions"
	"github.com/anggasct/aura/internal/platform/turn/turntest"
	"github.com/anggasct/aura/internal/store"
)

func collectDurableTerminal(t *testing.T, db *sql.DB, turnID string) []store.RuntimeEvent {
	t.Helper()
	waitForTerminalDurable(t, db, turnID)
	events, err := store.NewDedupeStore(db).ListTurnEvents(context.Background(), acceptedSessionOf(t, db, turnID), turnID)
	if err != nil {
		t.Fatalf("list turn events: %v", err)
	}
	return events
}

func TestDurableAdmitCompletesTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := turntest.NewFakeExecutor([]turntest.FakeStep{
			{Kind: turn.EventKindModelStarted, Payload: []byte(`{}`)},
			{Kind: turn.EventKindMessageCompleted, Payload: []byte(`{}`)},
		})
		engine, _, db := newDurableLiveTestRuntime(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
		mustCreateSession(t, db, "session-a")

		events, err := collect(t, engine, sampleRequest("session-a", "turn-0"))
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if last := events[len(events)-1]; last.Kind != turn.EventKindTurnCompleted {
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
		executor := turntest.NewFakeExecutor([]turntest.FakeStep{
			{Kind: turn.EventKindModelStarted, Payload: []byte(`{}`), Block: gate},
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
	})
}

func TestDurableDuplicateKeyReplaysWithoutReexecution(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := turntest.NewFakeExecutor([]turntest.FakeStep{
			{Kind: turn.EventKindModelStarted, Payload: []byte(`{}`)},
		})
		engine, _, db := newDurableLiveTestRuntime(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
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
		executor := turntest.NewFakeExecutor([]turntest.FakeStep{
			{Kind: turn.EventKindModelStarted, Payload: []byte(`{}`), Block: gate},
		})
		engine, _, db := newDurableLiveTestRuntime(t, Config{MaxActiveTurns: 1, MaxPendingTurns: 1}, executor)
		mustCreateSession(t, db, "session-a")

		go func() { _, _ = collect(t, engine, sampleRequest("session-a", "turn-0")) }()
		waitFor(t, func() bool { return executor.StartCount() == 1 })
		_, err := collect(t, engine, sampleRequest("session-a", "turn-1"))
		if err == nil {
			t.Fatal("expected an overload error")
		}
		code, ok := turn.CodeOf(err)
		if !ok || code != turn.ErrorCodeRuntimeOverloaded {
			t.Fatalf("code = %v, want runtime_overloaded", err)
		}
		if got := acceptedCount(t, db, "session-a"); got != 1 {
			t.Fatalf("accepted events = %d, want 1 (overloaded admit must persist nothing)", got)
		}
		rejected, err := store.NewDedupeStore(db).ListTurnEvents(context.Background(), "session-a", "turn-1")
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
		script := []turntest.FakeStep{
			{Kind: turn.EventKindModelStarted, Payload: []byte(`{}`)},
			{Kind: turn.EventKindModelDelta, Payload: []byte(`{}`)},
			{Kind: turn.EventKindMessageCompleted, Payload: []byte(`{}`)},
		}
		requests := func() []*turn.TurnRequest {
			return []*turn.TurnRequest{
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
		legacyExecutor := turntest.NewFakeExecutor(script)
		legacy, legacyDB, _ := newTestRuntime(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, legacyExecutor)
		mustCreateSession(t, legacyDB, "session-a")
		mustCreateSession(t, legacyDB, "session-b")
		legacyTranscripts := runAll(legacy, legacyDB)

		durableExecutor := turntest.NewFakeExecutor(script)
		durableEngine, _, durableDB := newDurableLiveTestRuntime(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, durableExecutor)
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
		executor := turntest.NewFakeExecutor([]turntest.FakeStep{
			{Kind: turn.EventKindModelStarted, Payload: []byte(`{}`), Block: gate},
		})
		engine, _, db := newDurableLiveTestRuntime(t, Config{MaxActiveTurns: 1, MaxPendingTurns: 16, ShutdownTimeout: 5 * time.Second}, executor)
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
		if last := events[len(events)-1]; last.Kind != turn.EventKindTurnCancelled {
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

func TestDurableDescriptorKeepsTheTurnTaint(t *testing.T) {
	tests := map[string]struct {
		origin    turn.Origin
		knowledge []string
		recall    *turn.UntrustedRecall
		want      bool
	}{
		"webhook":           {origin: turn.OriginWebhook, want: true},
		"http with history": {origin: turn.OriginHTTP, recall: &turn.UntrustedRecall{Trust: approval.TrustUntrustedExternal}, want: true},
		"knowledge":         {origin: turn.OriginTerminal, knowledge: []string{"faq"}, want: true},
		"schedule":          {origin: turn.OriginSchedule},
		"terminal":          {origin: turn.OriginTerminal},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			req := sampleRequest("session-a", "turn-0")
			req.Origin = tc.origin
			req.Knowledge = tc.knowledge
			req.UntrustedContext = tc.recall
			encoded, err := json.Marshal(turnDescriptorFromRequest(req))
			if err != nil {
				t.Fatalf("marshal descriptor: %v", err)
			}
			var desc runtimesessions.Descriptor
			if err := json.Unmarshal(encoded, &desc); err != nil {
				t.Fatalf("unmarshal descriptor: %v", err)
			}
			if got := turnRequestFromDescriptor(&desc).Tainted(); got != req.Tainted() || got != tc.want {
				t.Errorf("Tainted() after the round trip = %v, request %v, want %v", got, req.Tainted(), tc.want)
			}
		})
	}
}

func TestDurableLongStreamReplaysFullyFromStore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const deltas = 100
		script := make([]turntest.FakeStep, 0, deltas+1)
		for i := range deltas {
			script = append(script, turntest.FakeStep{
				Kind:    turn.EventKindModelDelta,
				Payload: []byte(`{"n":` + string(rune('0'+i%10)) + `}`),
			})
		}
		script = append(script, turntest.FakeStep{Kind: turn.EventKindMessageCompleted, Payload: []byte(`{}`)})
		executor := turntest.NewFakeExecutor(script)
		engine, _, db := newDurableLiveTestRuntime(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
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
		stored, err := store.NewDedupeStore(db).ListTurnEvents(context.Background(), "session-a", "turn-0")
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
	script := []turntest.FakeStep{
		{Kind: turn.EventKindModelStarted, Payload: []byte(`{"s":1}`)},
		{Kind: turn.EventKindModelDelta, Payload: []byte(`{"d":1}`)},
		{Kind: turn.EventKindModelDelta, Payload: []byte(`{"d":2}`)},
		{Kind: turn.EventKindMessageCompleted, Payload: []byte(`{}`)},
	}
	executor := turntest.NewFakeExecutor(script)
	engine, _, db := newDurableLiveTestRuntime(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
	mustCreateSession(t, db, "session-live")

	streamed, err := collect(t, engine, sampleRequest("session-live", "turn-live-0"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	stored, err := store.NewDedupeStore(db).ListTurnEvents(context.Background(), "session-live", "turn-live-0")
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
	executor := turntest.NewFakeExecutor([]turntest.FakeStep{
		{Kind: turn.EventKindModelStarted, Payload: []byte(`{}`), Block: gate},
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
		if last := events[len(events)-1]; last.Kind != turn.EventKindTurnCompleted {
			t.Fatalf("turn %d terminal = %q, want turn.completed", i, last.Kind)
		}
	}
}

func TestDurableResumeNeedsNoScan(t *testing.T) {
	executor := turntest.NewFakeExecutor([]turntest.FakeStep{
		{Kind: turn.EventKindModelStarted, Payload: []byte(`{}`)},
	})
	_, db, events := newTestRuntime(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
	mustCreateSession(t, db, "session-a")
	sessions := newHookedSessionStore(t, db)
	backend := durable.NewFake()
	ctx := context.Background()
	for _, turnRequest := range []string{"turn-0", "turn-1"} {
		desc := turnDescriptorFromRequest(sampleRequest("session-a", turnRequest))
		if _, err := sessions.Admit(ctx, &runtimesessions.AdmitRequest{
			Turn:            desc,
			AcceptedEventID: "event-" + turnRequest,
			MaxPending:      16,
		}); err != nil {
			t.Fatalf("preload admit %s: %v", turnRequest, err)
		}
	}
	restarted, err := NewEngine(&Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, events, store.NewDedupeStore(db), executor, nil)
	if err != nil {
		t.Fatalf("restarted engine: %v", err)
	}
	restarted.sessionStore = sessions
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
	for _, turnRequest := range []string{"turn-0", "turn-1"} {
		events := collectDurableTerminal(t, db, turnRequest)
		if last := events[len(events)-1]; last.Kind != turn.EventKindTurnCompleted {
			t.Fatalf("%s terminal = %q, want turn.completed", turnRequest, last.Kind)
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
	executor := turntest.NewFakeExecutor([]turntest.FakeStep{{Kind: turn.EventKindModelStarted, Payload: []byte(`{}`), Block: gate}})
	engine, _, db := newDurableLiveTestRuntime(t, Config{
		MaxActiveTurns: 2, MaxPendingTurns: 4, TurnTimeout: 200 * time.Millisecond,
	}, executor)
	mustCreateSession(t, db, "session-a")

	events, err := collect(t, engine, sampleRequest("session-a", "turn-deadline"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	last := events[len(events)-1]
	if last.Kind != turn.EventKindTurnFailed {
		t.Fatalf("terminal kind = %q, want turn.failed", last.Kind)
	}
	var payload struct {
		Code turn.ErrorCode `json:"code"`
	}
	if err := json.Unmarshal(last.Payload, &payload); err != nil {
		t.Fatalf("decode terminal payload: %v", err)
	}
	if payload.Code != turn.ErrorCodeTurnDeadlineExceeded {
		t.Fatalf("terminal code = %q, want turn_deadline_exceeded", payload.Code)
	}
	var count int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM runtime_event WHERE turn_id = 'turn-deadline' AND kind = ?`,
		turn.EventKindTurnFailed).Scan(&count); err != nil {
		t.Fatalf("count terminal: %v", err)
	}
	if count != 1 {
		t.Fatalf("durable deadline terminals = %d, want 1", count)
	}
}

type errorExecutor struct {
	err error
}

func (e errorExecutor) Execute(_ context.Context, _ *turn.TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
	return func(yield func(store.RuntimeEvent, error) bool) {
		yield(store.RuntimeEvent{}, e.err)
	}
}

func TestDurableBudgetExhaustedIsTerminal(t *testing.T) {
	executor := errorExecutor{err: &turn.Error{Code: turn.ErrorCodeBudgetExhausted, Detail: "test budget"}}
	engine, _, db := newDurableLiveTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, executor)
	mustCreateSession(t, db, "session-a")

	events, err := collect(t, engine, sampleRequest("session-a", "turn-budget"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	last := events[len(events)-1]
	if last.Kind != turn.EventKindTurnFailed {
		t.Fatalf("terminal kind = %q, want turn.failed", last.Kind)
	}
	var payload struct {
		Code turn.ErrorCode `json:"code"`
	}
	if err := json.Unmarshal(last.Payload, &payload); err != nil {
		t.Fatalf("decode terminal payload: %v", err)
	}
	if payload.Code != turn.ErrorCodeBudgetExhausted {
		t.Fatalf("terminal code = %q, want budget_exhausted", payload.Code)
	}
	var count int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM runtime_event WHERE turn_id = 'turn-budget' AND kind = ?`,
		turn.EventKindTurnFailed).Scan(&count); err != nil {
		t.Fatalf("count terminal: %v", err)
	}
	if count != 1 {
		t.Fatalf("durable budget terminals = %d, want 1", count)
	}
}

func TestDurableCancelIsTerminal(t *testing.T) {
	gate := make(chan struct{})
	executor := turntest.NewFakeExecutor([]turntest.FakeStep{{Kind: turn.EventKindModelStarted, Payload: []byte(`{}`), Block: gate}})
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
			if ev.Kind != turn.EventKindTurnCancelled {
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
		turn.EventKindTurnCancelled).Scan(&count); err != nil {
		t.Fatalf("count terminal: %v", err)
	}
	if count != 1 {
		t.Fatalf("durable cancel terminals = %d, want 1", count)
	}
	next, err := collect(t, engine, sampleRequest("session-a", "turn-next"))
	if err != nil {
		t.Fatalf("turn after cancel: %v", err)
	}
	if last := next[len(next)-1]; last.Kind != turn.EventKindTurnCompleted {
		t.Fatalf("next terminal = %q, want turn.completed (cancel must advance the queue)", last.Kind)
	}
}

func TestDurableShutdownDrainsActive(t *testing.T) {
	gate := make(chan struct{})
	executor := turntest.NewFakeExecutor([]turntest.FakeStep{{Kind: turn.EventKindModelStarted, Payload: []byte(`{}`), Block: gate}})
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
	if last := events[len(events)-1]; last.Kind != turn.EventKindTurnCompleted {
		t.Fatalf("drained terminal = %q, want turn.completed", last.Kind)
	}
}

func TestDurableTurnReleasedBeforeItIsStagedStillReachesItsCaller(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := make(chan struct{})
		executor := turntest.NewFakeExecutor([]turntest.FakeStep{
			{Kind: turn.EventKindModelStarted, Payload: []byte(`{}`), Block: gate},
			{Kind: turn.EventKindMessageCompleted, Payload: []byte(`{}`)},
		})
		engine, sessions, db := newDurableLiveTestRuntime(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
		mustCreateSession(t, db, "session-a")
		sessions.hooks(func(s *hookedSessionStore) {
			s.afterAdmit = func(turnID string) {
				if turnID != "turn-1" {
					return
				}
				close(gate)
				synctest.Wait()
			}
		})
		first := make(chan error, 1)
		go func() {
			_, err := collect(t, engine, sampleRequest("session-a", "turn-0"))
			first <- err
		}()
		waitFor(t, func() bool { return executor.StartCount() == 1 })
		type streamed struct {
			events []store.RuntimeEvent
			err    error
		}
		second := make(chan streamed, 1)
		go func() {
			events, err := collect(t, engine, sampleRequest("session-a", "turn-1"))
			second <- streamed{events: events, err: err}
		}()
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if starts := executor.StartCount(); starts != 2 {
			t.Fatalf("executor starts before the next status poll = %d, want turn-1 started by its own grant", starts)
		}
		got := <-second
		events, err := got.events, got.err
		if err != nil {
			t.Fatalf("turn-1: %v", err)
		}
		if err := <-first; err != nil {
			t.Fatalf("turn-0: %v", err)
		}
		if last := events[len(events)-1]; last.Kind != turn.EventKindTurnCompleted {
			t.Fatalf("turn-1 ended with %q, want turn.completed", last.Kind)
		}
		if starts := executor.StartCount(); starts != 2 {
			t.Fatalf("executor starts = %d, want each turn once", starts)
		}
	})
}

func TestShutdownBetweenGrantAndStagingCancelsTheTurnAndLeavesNoGrant(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := make(chan struct{})
		executor := turntest.NewFakeExecutor([]turntest.FakeStep{
			{Kind: turn.EventKindModelStarted, Payload: []byte(`{}`), Block: gate},
			{Kind: turn.EventKindMessageCompleted, Payload: []byte(`{}`)},
		})
		engine, sessions, db := newDurableLiveTestRuntime(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16, ShutdownTimeout: 5 * time.Second}, executor)
		mustCreateSession(t, db, "session-a")
		sessions.hooks(func(s *hookedSessionStore) {
			s.afterAdmit = func(turnID string) {
				if turnID != "turn-1" {
					return
				}
				close(gate)
				synctest.Wait()
				if err := engine.Shutdown(context.Background()); err != nil {
					t.Errorf("shutdown: %v", err)
				}
			}
		})
		go func() { _, _ = collect(t, engine, sampleRequest("session-a", "turn-0")) }()
		waitFor(t, func() bool { return executor.StartCount() == 1 })
		events, _ := collect(t, engine, sampleRequest("session-a", "turn-1"))
		if len(events) == 0 || events[len(events)-1].Kind != turn.EventKindTurnCancelled {
			t.Fatalf("turn-1 events = %+v, want it cancelled by the shutdown", events)
		}
		engine.mu.Lock()
		defer engine.mu.Unlock()
		if len(engine.granted) != 0 || len(engine.admitting) != 0 || executor.StartCount() != 1 {
			t.Fatalf("granted = %v admitting = %v starts = %d", engine.granted, engine.admitting, executor.StartCount())
		}
	})
}

func TestReleaseNeverGrantsAnAdmittingTurnOfAnotherSession(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := turntest.NewFakeExecutor([]turntest.FakeStep{{Kind: turn.EventKindMessageCompleted, Payload: []byte(`{}`)}})
		engine, sessions, _ := newDurableLiveTestRuntime(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
		sessions.hooks(func(s *hookedSessionStore) {
			s.aroundStatus = func(string, func() (runtimesessions.StatusResult, error)) (runtimesessions.StatusResult, error) {
				foreign := runtimesessions.Descriptor{TurnID: "turn-x", SessionID: "session-b"}
				return runtimesessions.StatusResult{ActiveTurnID: "turn-x", Active: &runtimesessions.ActiveTurn{Descriptor: foreign}}, nil
			}
		})
		engine.mu.Lock()
		engine.admitting = map[string]string{"turn-x": "session-b"}
		engine.mu.Unlock()
		engine.releaseTurn(context.Background(), &turnState{req: turn.TurnRequest{SessionID: "session-a"}, turnID: "turn-0"})
		synctest.Wait()
		engine.mu.Lock()
		defer engine.mu.Unlock()
		if _, granted := engine.granted["turn-x"]; granted || executor.StartCount() != 0 {
			t.Fatalf("a turn of another session was granted (%v) or started (%d)", engine.granted, executor.StartCount())
		}
	})
}

func TestDurableTurnIgnoresAnotherSessionsEventsUnderItsTurnID(t *testing.T) {
	executor := newHoldingExecutor()
	gate := executor.hold("turn-1")
	rig := newDurableHostRig(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 16}, executor)
	mustCreateSession(t, rig.db, "session-a")
	mustCreateSession(t, rig.db, "session-b")
	rig.startHost()

	first := make(chan error, 1)
	go func() {
		_, err := collect(t, rig.engine, sampleRequest("session-a", "turn-1"))
		first <- err
	}()
	executor.waitEntered(t, "turn-1")
	type streamed struct {
		events []store.RuntimeEvent
		err    error
	}
	second := make(chan streamed, 1)
	go func() {
		events, err := collect(t, rig.engine, sampleRequest("session-a", "turn-2"))
		second <- streamed{events: events, err: err}
	}()
	eventually(t, "turn-2 waits behind turn-1", func() bool {
		_, queued := storeActiveTurn(t, rig.sessions, "session-a")
		return queued == 1
	})
	foreign := &store.RuntimeEvent{
		ID: "session-b-turn-2-completed", SessionID: "session-b", Sequence: 1, TurnID: "turn-2", InvocationID: "foreign",
		Author: "runtime", Kind: turn.EventKindTurnCompleted, SchemaVersion: 1, Payload: []byte(`{}`), CreatedAt: time.Now().UTC(),
	}
	if err := store.NewEventStore(rig.db).Append(t.Context(), foreign); err != nil {
		t.Fatalf("append the foreign event: %v", err)
	}
	close(gate)

	if err := <-first; err != nil {
		t.Fatalf("turn-1: %v", err)
	}
	got := <-second
	if got.err != nil {
		t.Fatalf("turn-2: %v", got.err)
	}
	if len(got.events) == 0 {
		t.Fatal("turn-2 streamed no events")
	}
	for i := range got.events {
		if got.events[i].SessionID != "session-a" {
			t.Fatalf("turn-2 streamed %s of session %q, want only session-a", got.events[i].Kind, got.events[i].SessionID)
		}
	}
	if last := got.events[len(got.events)-1]; last.Kind != turn.EventKindTurnCompleted {
		t.Fatalf("turn-2 ended with %s, want turn.completed", last.Kind)
	}
	if starts := executor.startCount("turn-2"); starts != 1 {
		t.Fatalf("executor started turn-2 %d times, want once", starts)
	}
}
