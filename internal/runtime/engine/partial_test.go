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

	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/runtime/channelhost"
	"github.com/anggasct/aura/internal/store"
)

func adkPartialStep(text string) runtime.FakeStep {
	payload := json.RawMessage(`{"content":{"role":"model","parts":[{"text":` + strconv.Quote(text) + `}]},"partial":true}`)
	return runtime.FakeStep{Kind: store.EventKindADK, Payload: payload}
}

func adkFinalStep(text string) runtime.FakeStep {
	payload := json.RawMessage(`{"content":{"role":"model","parts":[{"text":` + strconv.Quote(text) + `}]},"finishReason":"STOP"}`)
	return runtime.FakeStep{Kind: store.EventKindADK, Payload: payload}
}

func kindsOf(events []store.RuntimeEvent) []string {
	kinds := make([]string, 0, len(events))
	for i := range events {
		kinds = append(kinds, events[i].Kind)
	}
	return kinds
}

func decodeDeltas(t *testing.T, events []store.RuntimeEvent) []runtime.ModelDelta {
	t.Helper()
	var deltas []runtime.ModelDelta
	for i := range events {
		if events[i].Kind != runtime.EventKindModelDelta {
			continue
		}
		delta, ok := runtime.DecodeModelDelta(events[i].Payload)
		if !ok {
			t.Fatalf("delta payload %s does not decode", events[i].Payload)
		}
		deltas = append(deltas, delta)
	}
	return deltas
}

func TestPartialModelTextReachesSubscribersOnlyAsDeltas(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := runtime.NewFakeExecutor([]runtime.FakeStep{adkPartialStep("Hel"), adkPartialStep("lo"), adkFinalStep("Hello")})
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, executor)
		mustCreateSession(t, db, "session-a")

		live, err := collect(t, engine, sampleRequest("session-a", "turn-partial"))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}

		wantKinds := []string{runtime.EventKindTurnAccepted, runtime.EventKindModelDelta, runtime.EventKindModelDelta, store.EventKindADK, runtime.EventKindTurnCompleted}
		if got := kindsOf(live); !slices.Equal(got, wantKinds) {
			t.Fatalf("live kinds = %v, want %v", got, wantKinds)
		}
		wantDeltas := []runtime.ModelDelta{{Text: "Hel", Index: 0}, {Text: "lo", Index: 1}}
		if got := decodeDeltas(t, live); !slices.Equal(got, wantDeltas) {
			t.Errorf("deltas = %+v, want %+v", got, wantDeltas)
		}
		for i := range live {
			if live[i].Kind == runtime.EventKindModelDelta && live[i].Sequence != 0 {
				t.Errorf("delta carries sequence %d, want none", live[i].Sequence)
			}
		}

		stored, err := store.NewDedupeStore(db).ListTurnEvents(context.Background(), "turn-partial")
		if err != nil {
			t.Fatalf("ListTurnEvents: %v", err)
		}
		wantStored := []string{runtime.EventKindTurnAccepted, store.EventKindADK, runtime.EventKindTurnCompleted}
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
		executor := runtime.NewFakeExecutor([]runtime.FakeStep{adkPartialStep("par"), adkPartialStep("tial"), adkFinalStep("final answer")})
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
		executor := runtime.NewFakeExecutor([]runtime.FakeStep{adkPartialStep(""), adkFinalStep("done")})
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
		script := make([]runtime.FakeStep, 0, partials+1)
		for i := range partials {
			script = append(script, adkPartialStep("t"+strconv.Itoa(i)))
		}
		script = append(script, adkFinalStep("done"))
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, runtime.NewFakeExecutor(script))
		mustCreateSession(t, db, "session-a")

		next, stop := iter.Pull2(engine.Run(context.Background(), sampleRequest("session-a", "turn-slow")))
		defer stop()
		first, _, ok := next()
		if !ok || first.Kind != runtime.EventKindTurnAccepted {
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

		if last := rest[len(rest)-1]; last.Kind != runtime.EventKindTurnCompleted {
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

func (e *streamPartialExecutor) Execute(_ context.Context, req *runtime.TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
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
