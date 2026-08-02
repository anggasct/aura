package runtime

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// fakeChannelAdapter satisfies ChannelPort. Its only way to run work is the
// IngressSink handed to Start; when acceptEnv is set it submits that envelope
// through the sink exactly as a gateway adapter would.
type fakeChannelAdapter struct {
	name      string
	acceptEnv *IngressEnvelope

	startedCh chan struct{}
	stoppedCh chan struct{}
	accepted  chan struct{}

	mu        sync.Mutex
	acceptRef TurnRef
	acceptErr error
}

func newFakeChannelAdapter(name string) *fakeChannelAdapter {
	return &fakeChannelAdapter{
		startedCh: make(chan struct{}),
		stoppedCh: make(chan struct{}),
		accepted:  make(chan struct{}),
	}
}

func (a *fakeChannelAdapter) Start(ctx context.Context, sink IngressSink) error {
	close(a.startedCh)
	if a.acceptEnv != nil {
		ref, err := sink.Accept(ctx, a.acceptEnv)
		a.mu.Lock()
		a.acceptRef, a.acceptErr = ref, err
		a.mu.Unlock()
		close(a.accepted)
	}
	<-ctx.Done()
	close(a.stoppedCh)
	return ctx.Err()
}

func (a *fakeChannelAdapter) Deliver(_ context.Context, _ *DeliveryRequest) (ProviderReceipt, error) {
	return ProviderReceipt{ProviderID: a.name, At: time.Now().UTC()}, nil
}

func (a *fakeChannelAdapter) Health(_ context.Context) ChannelHealth {
	return ChannelHealth{Status: ChannelHealthy}
}

func (a *fakeChannelAdapter) ref() (TurnRef, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.acceptRef, a.acceptErr
}

func TestHostGatewayAdapterRunsWorkThroughSink(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := NewFakeExecutor([]FakeStep{jsonStep(EventKindMessageCompleted)})
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, executor)
		mustCreateSession(t, db, "conv-gw")

		adapter := newFakeChannelAdapter("telegram")
		adapter.acceptEnv = sampleEnvelope("conv-gw", "gw-msg-1")
		host, err := NewHost(engine, []ChannelPort{adapter}, nil)
		if err != nil {
			t.Fatalf("NewHost: %v", err)
		}
		if err := host.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		t.Cleanup(func() { _ = host.Shutdown(context.Background()) })

		<-adapter.accepted
		ref, acceptErr := adapter.ref()
		if acceptErr != nil {
			t.Fatalf("sink.Accept from adapter: %v", acceptErr)
		}
		if ref.Replayed || ref.TurnID == "" {
			t.Errorf("unexpected turn ref from adapter ingress: %+v", ref)
		}
		waitFor(t, func() bool { return terminalCount(t, db, ref.TurnID) == 1 })
		if got := eventCountByKind(t, db, ref.TurnID, EventKindTurnCompleted); got != 1 {
			t.Errorf("completed events = %d, want 1", got)
		}
	})
}

func TestHostShutdownStopsAdapters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := NewFakeExecutor([]FakeStep{jsonStep(EventKindMessageCompleted)})
		engine, _, _ := newTestRuntime(t, Config{ShutdownTimeout: time.Second}, executor)

		adapter := newFakeChannelAdapter("telegram")
		host, err := NewHost(engine, []ChannelPort{adapter}, nil)
		if err != nil {
			t.Fatalf("NewHost: %v", err)
		}
		if err := host.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		<-adapter.startedCh

		if err := host.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		select {
		case <-adapter.stoppedCh:
		default:
			t.Error("adapter Start did not return after shutdown")
		}
	})
}

func TestHostShutdownDrainsAcceptedTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := NewFakeExecutor([]FakeStep{jsonStep(EventKindMessageCompleted)})
		engine, db, _ := newTestRuntime(t, Config{ShutdownTimeout: time.Second}, executor)
		mustCreateSession(t, db, "conv-drain")

		adapter := newFakeChannelAdapter("telegram")
		adapter.acceptEnv = sampleEnvelope("conv-drain", "drain-1")
		host, err := NewHost(engine, []ChannelPort{adapter}, nil)
		if err != nil {
			t.Fatalf("NewHost: %v", err)
		}
		if err := host.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		<-adapter.accepted
		ref, acceptErr := adapter.ref()
		if acceptErr != nil {
			t.Fatalf("sink.Accept from adapter: %v", acceptErr)
		}

		if err := host.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		if got := terminalCount(t, db, ref.TurnID); got != 1 {
			t.Errorf("durable terminals after drain = %d, want 1", got)
		}
	})
}

func TestNewHostRequiresRuntime(t *testing.T) {
	_, err := NewHost(nil, nil, nil)
	code, ok := CodeOf(err)
	if !ok || code != ErrorCodeInvalidArgument {
		t.Fatalf("code = %q (ok=%v), want invalid_argument", code, ok)
	}
}
