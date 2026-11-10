package effect

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/store"
)

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

type channelPublisher struct {
	events chan *store.RuntimeEvent
}

func (p *channelPublisher) Publish(ev *store.RuntimeEvent) {
	select {
	case p.events <- ev:
	default:
	}
}

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

type recordingPublisher struct {
	mu     sync.Mutex
	events []store.RuntimeEvent
}

func (p *recordingPublisher) Publish(ev *store.RuntimeEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, *ev)
}

func TestFailedPrepareIsNeverPublished(t *testing.T) {
	t.Parallel()
	journal, db := newTestJournal(t)
	publisher := &recordingPublisher{}
	journal.SetEventPublisher(publisher)
	first := mustPrepare(t, journal, validPrepare(1))

	conflicting := validPrepare(2)
	conflicting.EventSequence = 1
	if _, err := journal.Prepare(context.Background(), conflicting); err == nil {
		t.Fatal("Prepare accepted a second intent at an occupied event sequence")
	}

	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if len(publisher.events) != 1 || publisher.events[0].Kind != EventKindToolRequested || publisher.events[0].TurnID != first.TurnID {
		t.Fatalf("published = %+v, want only the first tool.requested", publisher.events)
	}
	if got := intentCount(t, db, "idempotency_key = ?", conflicting.IdempotencyKey); got != 0 {
		t.Fatalf("intents for the failed prepare = %d, want 0", got)
	}
}
