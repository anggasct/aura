package effect

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/store"
)

// blockingProvider blocks inside Invoke until released, recording that the
// provider was reached.
type blockingProvider struct {
	entered chan struct{}
	release chan struct{}
}

func (p *blockingProvider) SupportsIdempotency() bool { return true }

func (p *blockingProvider) Invoke(ctx context.Context, inv *Invocation) (Outcome, error) {
	close(p.entered)
	select {
	case <-p.release:
		return Outcome{Succeeded: true, Receipt: json.RawMessage(`{}`)}, nil
	case <-ctx.Done():
		return Outcome{}, ctx.Err()
	}
}

// channelPublisher surfaces published events as they arrive.
type channelPublisher struct {
	events chan *store.RuntimeEvent
}

func (p *channelPublisher) Publish(ev *store.RuntimeEvent) {
	select {
	case p.events <- ev:
	default:
	}
}

// TestPreparePublishesToolRequestBeforeProviderInvocation proves the live
// delivery contract: the committed tool request reaches the runtime
// subscriber while the provider is still running, not after it settles. A
// provider that never releases must not delay the event.
func TestPreparePublishesToolRequestBeforeProviderInvocation(t *testing.T) {
	journal, _ := newTestJournal(t)
	published := make(chan *store.RuntimeEvent, 1)
	journal.SetEventPublisher(&channelPublisher{events: published})
	executor, err := NewExecutor(journal)
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	provider := &blockingProvider{entered: make(chan struct{}), release: make(chan struct{})}

	done := make(chan error, 1)
	go func() {
		intent, err := executor.Execute(context.Background(), &PrepareRequest{
			SessionID: "sess-1", TurnID: "turn-1", ToolCallID: "call-1",
			IdempotencyKey: "idem-1", Provider: "builtin-tools", Operation: "exec",
			Classification: ClassificationEffectful,
			Request:        json.RawMessage(`{"cmd":"true"}`),
			EventSequence:  1,
		}, provider)
		if err == nil && intent.State != StateSucceeded {
			err = codedError(ErrorCodeUnknown, "unexpected terminal state "+string(intent.State), nil)
		}
		done <- err
	}()

	// The provider stays blocked for the whole assertion: the event must
	// arrive without releasing it.
	select {
	case ev := <-published:
		if ev.Kind != EventKindToolRequested {
			t.Fatalf("published kind = %q, want %q", ev.Kind, EventKindToolRequested)
		}
		if ev.TurnID != "turn-1" || ev.Sequence != 1 {
			t.Fatalf("published event = %+v, want turn-1 at sequence 1", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tool.requested did not reach the subscriber while the provider was still running")
	}

	// The provider is entered only after the request was committed and
	// published; release it and let the execution settle.
	select {
	case <-provider.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("provider was never invoked")
	}
	close(provider.release)
	if err := <-done; err != nil {
		t.Fatalf("Execute: %v", err)
	}
}
