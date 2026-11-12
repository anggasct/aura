package runtimeengine

import (
	"context"
	"encoding/json"
	"iter"
	"slices"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/platform/turn/channelhost"
	runtimesessions "github.com/anggasct/aura/internal/platform/turn/sessions"
	"github.com/anggasct/aura/internal/store"
)

func adkPartialStep(text string) turn.FakeStep {
	payload := json.RawMessage(`{"content":{"role":"model","parts":[{"text":` + strconv.Quote(text) + `}]},"partial":true}`)
	return turn.FakeStep{Kind: store.EventKindADK, Payload: payload}
}

func adkFinalStep(text string) turn.FakeStep {
	payload := json.RawMessage(`{"content":{"role":"model","parts":[{"text":` + strconv.Quote(text) + `}]},"finishReason":"STOP"}`)
	return turn.FakeStep{Kind: store.EventKindADK, Payload: payload}
}

func kindsOf(events []store.RuntimeEvent) []string {
	kinds := make([]string, 0, len(events))
	for i := range events {
		kinds = append(kinds, events[i].Kind)
	}
	return kinds
}

func decodeDeltas(t *testing.T, events []store.RuntimeEvent) []turn.ModelDelta {
	t.Helper()
	var deltas []turn.ModelDelta
	for i := range events {
		if events[i].Kind != turn.EventKindModelDelta {
			continue
		}
		delta, ok := turn.DecodeModelDelta(events[i].Payload)
		if !ok {
			t.Fatalf("delta payload %s does not decode", events[i].Payload)
		}
		deltas = append(deltas, delta)
	}
	return deltas
}

func TestPartialModelTextReachesSubscribersOnlyAsDeltas(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := turn.NewFakeExecutor([]turn.FakeStep{adkPartialStep("Hel"), adkPartialStep("lo"), adkFinalStep("Hello")})
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, executor)
		mustCreateSession(t, db, "session-a")

		live, err := collect(t, engine, sampleRequest("session-a", "turn-partial"))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}

		wantKinds := []string{turn.EventKindTurnAccepted, turn.EventKindModelDelta, turn.EventKindModelDelta, store.EventKindADK, turn.EventKindTurnCompleted}
		if got := kindsOf(live); !slices.Equal(got, wantKinds) {
			t.Fatalf("live kinds = %v, want %v", got, wantKinds)
		}
		wantDeltas := []turn.ModelDelta{{Text: "Hel", Index: 0}, {Text: "lo", Index: 1}}
		if got := decodeDeltas(t, live); !slices.Equal(got, wantDeltas) {
			t.Errorf("deltas = %+v, want %+v", got, wantDeltas)
		}
		for i := range live {
			if live[i].Kind == turn.EventKindModelDelta && live[i].Sequence != 0 {
				t.Errorf("delta carries sequence %d, want none", live[i].Sequence)
			}
		}

		stored, err := store.NewDedupeStore(db).ListTurnEvents(context.Background(), "session-a", "turn-partial")
		if err != nil {
			t.Fatalf("ListTurnEvents: %v", err)
		}
		wantStored := []string{turn.EventKindTurnAccepted, store.EventKindADK, turn.EventKindTurnCompleted}
		if got := kindsOf(stored); !slices.Equal(got, wantStored) {
			t.Fatalf("stored kinds = %v, want %v", got, wantStored)
		}
		for i := range stored {
			if i > 0 && stored[i].Sequence != stored[i-1].Sequence+1 {
				t.Errorf("stored sequence %d follows %d, want no gap", stored[i].Sequence, stored[i-1].Sequence)
			}
			if _, partial := partialModelText(&stored[i]); partial {
				t.Errorf("stored event %d is partial: %s", i, stored[i].Payload)
			}
		}
	})
}

func TestPartialModelTextStaysOutOfDeliveryAndPostTurnInput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := turn.NewFakeExecutor([]turn.FakeStep{adkPartialStep("par"), adkPartialStep("tial"), adkFinalStep("final answer")})
		hook := &recordingHook{}
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4, PostTurnHook: hook.fn}, executor)
		mustCreateSession(t, db, "channel:333")
		capture := &capturingAdapter{}
		engine.SetChannelAdapters([]runtimechannelhost.ChannelPort{capture})

		if _, err := collect(t, engine, channelTurnRequest("channel:333", "turn-partial-delivery")); err != nil {
			t.Fatalf("Run: %v", err)
		}

		if capture.count() != 1 {
			t.Fatalf("deliveries = %d, want 1", capture.count())
		}
		if parts := capture.first().Parts; len(parts) != 1 || parts[0].Text != "final answer" {
			t.Errorf("delivered parts = %+v, want only the final answer", parts)
		}
		hook.mu.Lock()
		defer hook.mu.Unlock()
		if len(hook.counts) != 1 || hook.counts[0] != 1 {
			t.Errorf("post-turn events = %v, want one executed event", hook.counts)
		}
	})
}

func TestEmptyPartialModelTextProducesNoDelta(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := turn.NewFakeExecutor([]turn.FakeStep{adkPartialStep(""), adkFinalStep("done")})
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, executor)
		mustCreateSession(t, db, "session-a")

		live, err := collect(t, engine, sampleRequest("session-a", "turn-empty-partial"))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}

		if got := decodeDeltas(t, live); len(got) != 0 {
			t.Errorf("deltas = %+v, want none", got)
		}
	})
}

func TestSlowSubscriberLosesDeltasButKeepsLifecycleEvents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const partials = 3 * subscriberBufferSize
		script := make([]turn.FakeStep, 0, partials+1)
		for i := range partials {
			script = append(script, adkPartialStep("t"+strconv.Itoa(i)))
		}
		script = append(script, adkFinalStep("done"))
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, turn.NewFakeExecutor(script))
		mustCreateSession(t, db, "session-a")

		next, stop := iter.Pull2(engine.Run(context.Background(), sampleRequest("session-a", "turn-slow")))
		defer stop()
		first, _, ok := next()
		if !ok || first.Kind != turn.EventKindTurnAccepted {
			t.Fatalf("first event = %+v, present=%v", first, ok)
		}
		waitForTerminalDurable(t, db, "turn-slow")

		var rest []store.RuntimeEvent
		for {
			ev, err, more := next()
			if !more {
				break
			}
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			rest = append(rest, ev)
		}

		if last := rest[len(rest)-1]; last.Kind != turn.EventKindTurnCompleted {
			t.Fatalf("last event = %q, want turn.completed", last.Kind)
		}
		if !slices.Contains(kindsOf(rest), store.EventKindADK) {
			t.Errorf("the completed message was lost: %v", kindsOf(rest))
		}
		deltas := decodeDeltas(t, rest)
		if len(deltas) == 0 || len(deltas) >= partials {
			t.Fatalf("surviving deltas = %d, want some but fewer than %d", len(deltas), partials)
		}
		for i := 1; i < len(deltas); i++ {
			if deltas[i].Index <= deltas[i-1].Index {
				t.Errorf("delta index %d follows %d, want increasing", deltas[i].Index, deltas[i-1].Index)
			}
		}
	})
}

type streamPartialExecutor struct {
	mu   sync.Mutex
	seen []bool
}

func (e *streamPartialExecutor) Execute(_ context.Context, req *turn.TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
	e.mu.Lock()
	e.seen = append(e.seen, req.StreamPartial)
	e.mu.Unlock()
	return func(func(store.RuntimeEvent, error) bool) {}
}

func TestStreamPartialReachesTheExecutor(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{name: "requested", want: true},
		{name: "not requested", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			executor := &streamPartialExecutor{}
			engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, executor)
			mustCreateSession(t, db, "session-a")
			req := sampleRequest("session-a", "turn-flag")
			req.StreamPartial = tc.want

			if _, err := collect(t, engine, req); err != nil {
				t.Fatalf("Run: %v", err)
			}

			executor.mu.Lock()
			defer executor.mu.Unlock()
			if len(executor.seen) != 1 || executor.seen[0] != tc.want {
				t.Errorf("executor saw StreamPartial %v, want [%v]", executor.seen, tc.want)
			}
		})
	}
}

func TestDescriptorCarriesStreamPartial(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{name: "requested", want: true},
		{name: "not requested", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := sampleRequest("session-a", "turn-0")
			req.StreamPartial = tc.want

			desc := turnDescriptorFromRequest(req)
			raw, err := json.Marshal(&desc)
			if err != nil {
				t.Fatalf("marshal descriptor: %v", err)
			}
			var decoded runtimesessions.Descriptor
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("unmarshal descriptor: %v", err)
			}

			if got := turnRequestFromDescriptor(&decoded).StreamPartial; got != tc.want {
				t.Errorf("StreamPartial = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDescriptorWithoutStreamPartialDecodesAsFalse(t *testing.T) {
	legacy := []byte(`{"turn_id":"turn-0","session_id":"session-a","principal_id":"user-1","origin":"http","parts":[{"text":"hello"}],"idempotency_key":"","max_tokens":0,"max_cost":0,"trace_parent":"","agent_id":""}`)
	var desc runtimesessions.Descriptor
	if err := json.Unmarshal(legacy, &desc); err != nil {
		t.Fatalf("unmarshal legacy descriptor: %v", err)
	}

	if turnRequestFromDescriptor(&desc).StreamPartial {
		t.Error("a descriptor written before the field existed asks for streaming")
	}
}

type gatedPartialExecutor struct {
	partials []turn.FakeStep
	final    turn.FakeStep
}

func (e *gatedPartialExecutor) Execute(ctx context.Context, req *turn.TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
	script := []turn.FakeStep{e.final}
	if req.StreamPartial {
		script = append(slices.Clone(e.partials), e.final)
	}
	return turn.NewFakeExecutor(script).Execute(ctx, req)
}

func TestDurableTurnRelaysPartialModelTextWithoutJournalingIt(t *testing.T) {
	executor := &gatedPartialExecutor{partials: []turn.FakeStep{adkPartialStep("Hel"), adkPartialStep("lo")}, final: adkFinalStep("Hello")}
	engine, _, db := newDurableLiveTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, executor)
	mustCreateSession(t, db, "session-live")
	req := sampleRequest("session-live", "turn-durable-partial")
	req.StreamPartial = true

	live, err := collect(t, engine, req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	wantDeltas := []turn.ModelDelta{{Text: "Hel", Index: 0}, {Text: "lo", Index: 1}}
	if got := decodeDeltas(t, live); !slices.Equal(got, wantDeltas) {
		t.Errorf("deltas = %+v, want %+v", got, wantDeltas)
	}
	lastDelta, terminal := -1, -1
	for i := range live {
		switch live[i].Kind {
		case turn.EventKindModelDelta:
			lastDelta = i
		case turn.EventKindTurnCompleted:
			terminal = i
		}
	}
	if lastDelta < 0 || lastDelta > terminal {
		t.Errorf("deltas did not arrive before the turn ended: %v", kindsOf(live))
	}
	stored, err := store.NewDedupeStore(db).ListTurnEvents(context.Background(), req.SessionID, req.TurnID)
	if err != nil {
		t.Fatalf("ListTurnEvents: %v", err)
	}
	wantStored := []string{turn.EventKindTurnAccepted, store.EventKindADK, turn.EventKindTurnCompleted}
	if got := kindsOf(stored); !slices.Equal(got, wantStored) {
		t.Errorf("stored kinds = %v, want %v", got, wantStored)
	}
	for i := range stored {
		if _, partial := partialModelText(&stored[i]); partial {
			t.Errorf("stored event %d is partial", i)
		}
	}
}

func TestDurableTurnWithoutStreamPartialRelaysNoDeltas(t *testing.T) {
	executor := &gatedPartialExecutor{partials: []turn.FakeStep{adkPartialStep("Hel"), adkPartialStep("lo")}, final: adkFinalStep("Hello")}
	engine, _, db := newDurableLiveTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, executor)
	mustCreateSession(t, db, "session-live")

	live, err := collect(t, engine, sampleRequest("session-live", "turn-durable-plain"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := decodeDeltas(t, live); len(got) != 0 {
		t.Errorf("deltas = %+v, want none without StreamPartial", got)
	}
}
