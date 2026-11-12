package runtimeengine

import (
	"context"
	"iter"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/store"
)

type stopClockExecutor struct {
	started chan struct{}
	release chan struct{}

	mu          sync.Mutex
	cancelledAt time.Time
	endedLive   bool
}

func newStopClockExecutor() *stopClockExecutor {
	return &stopClockExecutor{started: make(chan struct{}), release: make(chan struct{})}
}

func (x *stopClockExecutor) Execute(ctx context.Context, _ *turn.TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
	return func(func(store.RuntimeEvent, error) bool) {
		stop := context.AfterFunc(ctx, func() {
			x.mu.Lock()
			x.cancelledAt = time.Now()
			x.mu.Unlock()
		})
		defer stop()
		close(x.started)
		<-x.release
		x.mu.Lock()
		x.endedLive = ctx.Err() == nil
		x.mu.Unlock()
	}
}

type shutdownCase struct {
	deadline time.Duration
	endAfter time.Duration
}

type shutdownObservation struct {
	cancelled time.Duration
	returned  time.Duration
	endedLive bool
	err       error
}

const budgetTestTimeout = 30 * time.Second

func observeShutdown(t *testing.T, c *shutdownCase) shutdownObservation {
	t.Helper()
	executor := newStopClockExecutor()
	engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 1, MaxPendingTurns: 4, ShutdownTimeout: budgetTestTimeout}, executor)
	mustCreateSession(t, db, "session-a")
	go func() {
		_, _ = collect(t, engine, sampleRequest("session-a", "turn-0"))
	}()
	<-executor.started
	ctx := context.Background()
	if c.deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.deadline)
		defer cancel()
	}
	start := time.Now()
	if c.endAfter > 0 {
		time.AfterFunc(c.endAfter, func() { close(executor.release) })
	}
	err := engine.Shutdown(ctx)
	observed := shutdownObservation{returned: time.Since(start), err: err}
	if c.endAfter == 0 {
		close(executor.release)
	}
	synctest.Wait()
	executor.mu.Lock()
	defer executor.mu.Unlock()
	if !executor.cancelledAt.IsZero() {
		observed.cancelled = executor.cancelledAt.Sub(start)
	}
	observed.endedLive = executor.endedLive
	return observed
}

func requireRuntimeInternal(t *testing.T, err error) {
	t.Helper()
	if code, ok := turn.CodeOf(err); !ok || code != turn.ErrorCodeRuntimeInternal {
		t.Fatalf("Shutdown error = %v, want runtime_internal", err)
	}
}

func TestShutdownEndsWithinItsBudget(t *testing.T) {
	t.Run("a turn that ignores its cancellation", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			got := observeShutdown(t, &shutdownCase{})
			if got.cancelled != 20*time.Second || got.returned != 30*time.Second {
				t.Fatalf("turn cancelled after %v, Shutdown returned after %v; want 20s and 30s", got.cancelled, got.returned)
			}
			requireRuntimeInternal(t, got.err)
		})
	})
	t.Run("a deadline shorter than the timeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			got := observeShutdown(t, &shutdownCase{deadline: 8 * time.Second})
			if got.cancelled != 4*time.Second || got.returned != 8*time.Second {
				t.Fatalf("turn cancelled after %v, Shutdown returned after %v; want 4s and 8s", got.cancelled, got.returned)
			}
			requireRuntimeInternal(t, got.err)
		})
	})
	t.Run("a turn that ends within the drain", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			got := observeShutdown(t, &shutdownCase{endAfter: 19 * time.Second})
			if got.err != nil || got.returned != 19*time.Second || !got.endedLive {
				t.Fatalf("Shutdown = %v after %v, turn ended live %v; want nil after 19s, never cancelled", got.err, got.returned, got.endedLive)
			}
		})
	})
	t.Run("a turn that ends within the cancel reserve", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			got := observeShutdown(t, &shutdownCase{endAfter: 25 * time.Second})
			if got.err != nil || got.returned != 25*time.Second || got.cancelled != 20*time.Second {
				t.Fatalf("Shutdown = %v after %v, turn cancelled after %v; want nil after 25s, cancelled after 20s", got.err, got.returned, got.cancelled)
			}
		})
	})
}
