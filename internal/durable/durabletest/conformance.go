package durabletest

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/durable"
)

func waitForState(ctx context.Context, t *testing.T, backend durable.Runtime, key string, want ...durable.RunState) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, err := backend.Status(ctx, durable.RunRef{Key: key})
		if err != nil {
			t.Fatalf("Status(%q): %v", key, err)
		}
		for _, state := range want {
			if status.State == state {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %q state = %q, want %v", key, status.State, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func ExerciseRuntime(t *testing.T, backend durable.Runtime) {
	t.Helper()
	ctx := context.Background()
	registrar, ok := backend.(durable.HandlerRegistrar)
	if !ok {
		t.Fatal("backend does not register handlers")
	}

	var greetCalls atomic.Int64
	registrar.RegisterHandler("greet", func(_ context.Context, _ *durable.Invocation) error {
		greetCalls.Add(1)
		return nil
	})
	release := make(chan struct{})
	var waiterCalls atomic.Int64
	var mu sync.Mutex
	var woke []string
	registrar.RegisterHandler("waiter", func(ctx context.Context, inv *durable.Invocation) error {
		waiterCalls.Add(1)
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		payload, ok := inv.Signal(ctx, "wake")
		if !ok {
			return context.Canceled
		}
		mu.Lock()
		woke = append(woke, string(payload))
		mu.Unlock()
		return nil
	})

	t.Run("start_rejects_unknown_handler", func(t *testing.T) {
		if _, err := backend.Start(ctx, durable.StartRequest{Handler: "missing", Key: "k-unknown-handler"}); err == nil {
			t.Fatal("expected unknown handler to fail, got nil")
		}
	})

	t.Run("start_rejects_empty_key", func(t *testing.T) {
		if _, err := backend.Start(ctx, durable.StartRequest{Handler: "greet"}); err == nil {
			t.Fatal("expected empty key to fail, got nil")
		}
	})

	t.Run("start_runs_to_completion", func(t *testing.T) {
		ref, err := backend.Start(ctx, durable.StartRequest{Handler: "greet", Key: "k-complete", Payload: []byte(`{}`)})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if ref.Key != "k-complete" {
			t.Errorf("ref = %+v, want key k-complete", ref)
		}
		waitForState(ctx, t, backend, "k-complete", durable.RunSucceeded)
	})

	t.Run("duplicate_start_returns_same_run_once", func(t *testing.T) {
		first, err := backend.Start(ctx, durable.StartRequest{Handler: "greet", Key: "k-duplicate", Payload: []byte(`{}`)})
		if err != nil {
			t.Fatalf("first Start: %v", err)
		}
		second, err := backend.Start(ctx, durable.StartRequest{Handler: "greet", Key: "k-duplicate", Payload: []byte(`{}`)})
		if err != nil {
			t.Fatalf("second Start: %v", err)
		}
		if first != second {
			t.Errorf("duplicate Start refs differ: %+v vs %+v", first, second)
		}
		waitForState(ctx, t, backend, "k-duplicate", durable.RunSucceeded)
		if greetCalls.Load() != 2 {
			t.Errorf("greet handler ran %d times, want 2 (k-complete and k-duplicate once each)", greetCalls.Load())
		}
	})

	t.Run("signal_wakes_suspended_run", func(t *testing.T) {
		ref, err := backend.Start(ctx, durable.StartRequest{Handler: "waiter", Key: "k-wake", Payload: []byte(`{}`)})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		close(release)
		waitForState(ctx, t, backend, ref.Key, durable.RunRunning)
		if err := backend.Signal(ctx, ref, "wake", []byte(`{"up":true}`)); err != nil {
			t.Fatalf("Signal: %v", err)
		}
		waitForState(ctx, t, backend, ref.Key, durable.RunSucceeded)
		mu.Lock()
		defer mu.Unlock()
		if waiterCalls.Load() != 1 {
			t.Errorf("waiter handler ran %d times, want 1", waiterCalls.Load())
		}
		if len(woke) != 1 || woke[0] != `{"up":true}` {
			t.Errorf("woke payloads = %v, want the signal payload", woke)
		}
	})

	t.Run("signal_unknown_run_fails", func(t *testing.T) {
		err := backend.Signal(ctx, durable.RunRef{Key: "k-never-started"}, "wake", []byte(`{}`))
		if !errors.Is(err, durable.ErrUnknownRun) {
			t.Fatalf("signal unknown run err = %v, want %v", err, durable.ErrUnknownRun)
		}
	})

	t.Run("cancel_ends_running_run", func(t *testing.T) {
		held := make(chan struct{})
		registrar.RegisterHandler("hold", func(ctx context.Context, _ *durable.Invocation) error {
			select {
			case <-held:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		ref, err := backend.Start(ctx, durable.StartRequest{Handler: "hold", Key: "k-cancel", Payload: []byte(`{}`)})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		waitForState(ctx, t, backend, ref.Key, durable.RunRunning)
		if err := backend.Cancel(ctx, ref); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		close(held)
		waitForState(ctx, t, backend, ref.Key, durable.RunCancelled, durable.RunSucceeded, durable.RunFailed)
	})

	t.Run("cancel_terminal_run_succeeds", func(t *testing.T) {
		if err := backend.Cancel(ctx, durable.RunRef{Key: "k-complete"}); err != nil {
			t.Errorf("cancel terminal run: %v", err)
		}
	})

	t.Run("status_unknown_run_fails", func(t *testing.T) {
		if _, err := backend.Status(ctx, durable.RunRef{Key: "k-never-started"}); !errors.Is(err, durable.ErrUnknownRun) {
			t.Fatalf("status unknown run err = %v, want %v", err, durable.ErrUnknownRun)
		}
	})

	t.Run("cancel_unknown_run_fails", func(t *testing.T) {
		if err := backend.Cancel(ctx, durable.RunRef{Key: "k-never-started"}); !errors.Is(err, durable.ErrUnknownRun) {
			t.Fatalf("cancel unknown run err = %v, want %v", err, durable.ErrUnknownRun)
		}
	})

	t.Run("empty_payload_accepted", func(t *testing.T) {
		registrar.RegisterHandler("emptyhold", func(ctx context.Context, _ *durable.Invocation) error {
			<-ctx.Done()
			return ctx.Err()
		})
		startCases := []struct {
			name    string
			key     string
			payload []byte
		}{
			{"nil", "k-empty-nil", nil},
			{"empty", "k-empty-blank", []byte{}},
			{"object", "k-empty-object", []byte(`{}`)},
		}
		for _, tc := range startCases {
			if _, err := backend.Start(ctx, durable.StartRequest{Handler: "greet", Key: tc.key, Payload: tc.payload}); err != nil {
				t.Fatalf("Start with %s payload: %v", tc.name, err)
			}
			waitForState(ctx, t, backend, tc.key, durable.RunSucceeded)
		}
		ref, err := backend.Start(ctx, durable.StartRequest{Handler: "emptyhold", Key: "k-empty-signal", Payload: nil})
		if err != nil {
			t.Fatalf("Start emptyhold: %v", err)
		}
		waitForState(ctx, t, backend, ref.Key, durable.RunRunning)
		for _, tc := range startCases {
			if err := backend.Signal(ctx, ref, "wake", tc.payload); err != nil {
				t.Fatalf("Signal with %s payload: %v", tc.name, err)
			}
		}
		if err := backend.Cancel(ctx, ref); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		waitForState(ctx, t, backend, ref.Key, durable.RunCancelled, durable.RunSucceeded, durable.RunFailed)
	})
}
