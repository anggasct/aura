package runtimeengine

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/platform/turn/channelhost"
	"github.com/anggasct/aura/internal/platform/turn/ingress"
)

type fakeChannelAdapter struct {
	name      string
	acceptEnv *runtimeingress.IngressEnvelope

	startedCh chan struct{}
	stoppedCh chan struct{}
	accepted  chan struct{}

	mu        sync.Mutex
	acceptRef runtimeingress.TurnRef
	acceptErr error
}

func newFakeChannelAdapter(name string) *fakeChannelAdapter {
	return &fakeChannelAdapter{
		startedCh: make(chan struct{}),
		stoppedCh: make(chan struct{}),
		accepted:  make(chan struct{}),
	}
}

func (a *fakeChannelAdapter) Start(ctx context.Context, sink runtimeingress.IngressSink) error {
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

func (a *fakeChannelAdapter) Deliver(_ context.Context, _ *runtimechannelhost.DeliveryRequest) (runtimechannelhost.ProviderReceipt, error) {
	return runtimechannelhost.ProviderReceipt{ProviderID: a.name, At: time.Now().UTC()}, nil
}

func (a *fakeChannelAdapter) Health(_ context.Context) runtimechannelhost.ChannelHealth {
	return runtimechannelhost.ChannelHealth{Status: runtimechannelhost.ChannelHealthy}
}

func (a *fakeChannelAdapter) ref() (runtimeingress.TurnRef, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.acceptRef, a.acceptErr
}

func TestHostGatewayAdapterRunsWorkThroughSink(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := turn.NewFakeExecutor([]turn.FakeStep{jsonStep(turn.EventKindMessageCompleted)})
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, executor)
		mustCreateSession(t, db, sampleSession("conv-gw"))

		adapter := newFakeChannelAdapter("telegram")
		adapter.acceptEnv = sampleEnvelope("conv-gw", "gw-msg-1")
		host, err := NewHost(engine, []runtimechannelhost.ChannelPort{adapter}, nil)
		if err != nil {
			t.Fatalf("NewHost: %v", err)
		}
		if err := host.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		t.Cleanup(func() {
			_ = host.Shutdown(context.Background())
			_ = engine.Shutdown(context.Background())
		})

		<-adapter.accepted
		ref, acceptErr := adapter.ref()
		if acceptErr != nil {
			t.Fatalf("sink.Accept from adapter: %v", acceptErr)
		}
		if ref.Replayed || ref.TurnID == "" {
			t.Errorf("unexpected turn ref from adapter ingress: %+v", ref)
		}
		waitFor(t, func() bool { return terminalCount(t, db, ref.TurnID) == 1 })
		if got := eventCountByKind(t, db, ref.TurnID, turn.EventKindTurnCompleted); got != 1 {
			t.Errorf("completed events = %d, want 1", got)
		}
	})
}

func TestHostShutdownStopsAdapters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := turn.NewFakeExecutor([]turn.FakeStep{jsonStep(turn.EventKindMessageCompleted)})
		engine, db, _ := newTestRuntime(t, Config{ShutdownTimeout: time.Second}, executor)
		mustCreateSession(t, db, "session-a")

		adapter := newFakeChannelAdapter("telegram")
		host, err := NewHost(engine, []runtimechannelhost.ChannelPort{adapter}, nil)
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

		events, err := collect(t, engine, sampleRequest("session-a", "turn-after-host-stop"))
		if err != nil {
			t.Fatalf("turn after the host stopped: %v", err)
		}
		if got := lastKind(events); got != turn.EventKindTurnCompleted {
			t.Fatalf("turn after the host stopped ended with %q, want %q", got, turn.EventKindTurnCompleted)
		}
		if err := engine.Shutdown(context.Background()); err != nil {
			t.Fatalf("engine Shutdown: %v", err)
		}
	})
}

func TestHostShutdownDrainsAcceptedTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := make(chan struct{})
		executor := turn.NewFakeExecutor([]turn.FakeStep{{Kind: turn.EventKindMessageCompleted, Payload: []byte(`{}`), Block: gate}})
		engine, db, _ := newTestRuntime(t, Config{ShutdownTimeout: time.Second}, executor)
		mustCreateSession(t, db, sampleSession("conv-drain"))

		adapter := newFakeChannelAdapter("telegram")
		adapter.acceptEnv = sampleEnvelope("conv-drain", "drain-1")
		host, err := NewHost(engine, []runtimechannelhost.ChannelPort{adapter}, nil)
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
		synctest.Wait()
		if starts := executor.StartCount(); starts != 1 {
			t.Fatalf("executor started %d turns before the host stopped, want 1", starts)
		}

		if err := host.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		select {
		case <-adapter.stoppedCh:
		default:
			t.Fatal("adapter Start did not return after shutdown")
		}
		close(gate)
		if err := engine.Shutdown(context.Background()); err != nil {
			t.Fatalf("engine Shutdown: %v", err)
		}
		if got := terminalCount(t, db, ref.TurnID); got != 1 {
			t.Errorf("durable terminals after drain = %d, want 1", got)
		}
	})
}

func TestNewHostRequiresRuntime(t *testing.T) {
	_, err := NewHost(nil, nil, nil)
	code, ok := turn.CodeOf(err)
	if !ok || code != turn.ErrorCodeInvalidArgument {
		t.Fatalf("code = %q (ok=%v), want invalid_argument", code, ok)
	}
}
