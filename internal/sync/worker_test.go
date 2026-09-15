package sync

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func testWorker(pass PassFunc) (*Worker, *StateStore) {
	state := NewStateStore()
	worker, err := NewWorker(WorkerConfig{
		Interval: time.Hour,
		Clock:    time.Now().UTC,
		Jitter:   func() float64 { return 0.5 },
	}, state, pass)
	if err != nil {
		panic(err)
	}
	return worker, state
}

func TestWorkerNilArgs(t *testing.T) {
	t.Parallel()
	if _, err := NewWorker(WorkerConfig{Interval: time.Minute}, nil, func(_ context.Context) PassOutcome { return PassOutcome{} }); err == nil {
		t.Fatal("nil state must fail")
	}
	if _, err := NewWorker(WorkerConfig{Interval: time.Minute}, NewStateStore(), nil); err == nil {
		t.Fatal("nil pass must fail")
	}
	if _, err := NewWorker(WorkerConfig{}, NewStateStore(), func(_ context.Context) PassOutcome { return PassOutcome{} }); err == nil {
		t.Fatal("non-positive interval must fail")
	}
}

func TestWorkerCoalescesOverlap(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	finished := make(chan struct{}, 4)
	var calls atomic.Int64
	worker, state := testWorker(func(_ context.Context) PassOutcome {
		calls.Add(1)
		started <- struct{}{}
		<-release
		outcome := PassOutcome{State: WorkerIdle, Result: string(AdvanceUpToDate)}
		finished <- struct{}{}
		return outcome
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_ = worker.Start(ctx)
	defer worker.Stop()
	worker.Notify()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first pass did not start")
	}
	worker.Notify()
	worker.Notify()
	worker.Notify()
	close(release)
	for range 2 {
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Fatalf("calls = %d, want 2 coalesced passes", calls.Load())
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2 coalesced passes", calls.Load())
	}
	if got := state.Load(); got.State != WorkerIdle {
		t.Fatalf("state = %q, want idle", got.State)
	}
}

func TestWorkerBackoffResets(t *testing.T) {
	t.Parallel()
	worker, _ := testWorker(func(_ context.Context) PassOutcome { return PassOutcome{} })
	worker.noteRetryable(true)
	worker.noteRetryable(true)
	worker.mu.Lock()
	backoff := worker.backoff
	worker.mu.Unlock()
	if backoff != 2*baseBackoff {
		t.Fatalf("backoff = %v, want %v", backoff, 2*baseBackoff)
	}
	worker.noteRetryable(false)
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if worker.backoff != 0 {
		t.Fatalf("backoff = %v, want reset", worker.backoff)
	}
}

func TestWorkerBackoffGrowsAcrossPasses(t *testing.T) {
	t.Parallel()
	worker, _ := testWorker(func(_ context.Context) PassOutcome {
		return PassOutcome{State: WorkerUnknown, UnknownID: "intent-1", Result: string(WorkerUnknown), Retryable: true}
	})
	worker.runPass(context.Background())
	first := worker.Backoff()
	if first != baseBackoff {
		t.Fatalf("first backoff = %v, want %v", first, baseBackoff)
	}
	worker.runPass(context.Background())
	second := worker.Backoff()
	if second != 2*baseBackoff {
		t.Fatalf("second backoff = %v, want %v", second, 2*baseBackoff)
	}
	worker.mu.Lock()
	interval := worker.jitteredIntervalLocked()
	worker.mu.Unlock()
	base := time.Hour
	if interval <= base {
		t.Fatalf("jittered interval = %v, want larger than base %v", interval, base)
	}
}

func TestWorkerParentCancelStopsLoop(t *testing.T) {
	t.Parallel()
	worker, _ := testWorker(func(ctx context.Context) PassOutcome {
		select {
		case <-ctx.Done():
			return PassOutcome{State: WorkerUnknown, UnknownID: "cancelled", Result: string(WorkerUnknown), Retryable: true}
		default:
			return PassOutcome{State: WorkerIdle, Result: string(AdvanceUpToDate)}
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	_ = worker.Start(ctx)
	cancel()
	select {
	case <-worker.done:
	case <-time.After(5 * time.Second):
		t.Fatal("parent cancel did not terminate loop")
	}
	worker.Stop()
}

func TestWorkerStopWithoutStart(t *testing.T) {
	t.Parallel()
	worker, _ := testWorker(func(_ context.Context) PassOutcome { return PassOutcome{} })
	done := make(chan struct{})
	go func() {
		worker.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop without Start blocked")
	}
	worker.Stop()
}

func TestWorkerQueueBounded(t *testing.T) {
	t.Parallel()
	worker, _ := testWorker(func(_ context.Context) PassOutcome { return PassOutcome{} })
	worker.mu.Lock()
	worker.lease = true
	worker.mu.Unlock()
	for range maxQueueDepth + 4 {
		worker.mu.Lock()
		if worker.lease {
			if worker.queue < maxQueueDepth {
				worker.queue++
			}
		}
		worker.mu.Unlock()
	}
	if depth := worker.QueueDepth(); depth != maxQueueDepth {
		t.Fatalf("queue = %d, want bound %d", depth, maxQueueDepth)
	}
}
