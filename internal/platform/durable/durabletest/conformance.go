package durabletest

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
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
	registrar.RegisterHandler("greet", func(_ context.Context, _ durable.Invocation) error {
		greetCalls.Add(1)
		return nil
	})
	release := make(chan struct{})
	var waiterCalls atomic.Int64
	var mu sync.Mutex
	var woke []string
	registrar.RegisterHandler("waiter", func(ctx context.Context, inv durable.Invocation) error {
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

	t.Run("wait_receives_signal_before_timeout", func(t *testing.T) {
		awake := make(chan struct{})
		registrar.RegisterHandler("waitbound", func(ctx context.Context, inv durable.Invocation) error {
			close(awake)
			payload, timedOut, ok := inv.Wait(ctx, "ping", 5*time.Second)
			if !ok || timedOut || string(payload) != `{"up":true}` {
				t.Errorf("Wait = %q, timedOut %v, ok %v; want the signal payload", payload, timedOut, ok)
			}
			return nil
		})
		ref, err := backend.Start(ctx, durable.StartRequest{Handler: "waitbound", Key: "k-wait-signal", Payload: []byte(`{}`)})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		select {
		case <-awake:
		case <-time.After(10 * time.Second):
			t.Fatal("handler never started waiting")
		}
		if err := backend.Signal(ctx, ref, "ping", []byte(`{"up":true}`)); err != nil {
			t.Fatalf("Signal: %v", err)
		}
		waitForState(ctx, t, backend, ref.Key, durable.RunSucceeded)
	})

	t.Run("wait_times_out_without_signal", func(t *testing.T) {
		registrar.RegisterHandler("waitidle", func(ctx context.Context, inv durable.Invocation) error {
			_, timedOut, ok := inv.Wait(ctx, "never", 50*time.Millisecond)
			if !ok || !timedOut {
				t.Errorf("Wait = timedOut %v, ok %v; want a timeout", timedOut, ok)
			}
			return nil
		})
		ref, err := backend.Start(ctx, durable.StartRequest{Handler: "waitidle", Key: "k-wait-timeout", Payload: []byte(`{}`)})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		waitForState(ctx, t, backend, ref.Key, durable.RunSucceeded)
	})

	t.Run("run_action_returns_function_result", func(t *testing.T) {
		registrar.RegisterHandler("actor", func(ctx context.Context, inv durable.Invocation) error {
			out, err := inv.RunAction(ctx, "step", func(context.Context) ([]byte, error) {
				return []byte(`{"done":true}`), nil
			})
			if err != nil || string(out) != `{"done":true}` {
				t.Errorf("RunAction = %q, %v; want the function bytes", out, err)
			}
			if _, err := inv.RunAction(ctx, "step", nil); err == nil {
				t.Error("expected nil function to fail, got nil")
			}
			return nil
		})
		ref, err := backend.Start(ctx, durable.StartRequest{Handler: "actor", Key: "k-action", Payload: []byte(`{}`)})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		waitForState(ctx, t, backend, ref.Key, durable.RunSucceeded)
	})

	t.Run("signal_unknown_run_fails", func(t *testing.T) {
		err := backend.Signal(ctx, durable.RunRef{Key: "k-never-started"}, "wake", []byte(`{}`))
		if !errors.Is(err, durable.ErrUnknownRun) {
			t.Fatalf("signal unknown run err = %v, want %v", err, durable.ErrUnknownRun)
		}
	})

	t.Run("cancel_ends_running_run", func(t *testing.T) {
		held := make(chan struct{})
		registrar.RegisterHandler("hold", func(ctx context.Context, _ durable.Invocation) error {
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
		registrar.RegisterHandler("emptyhold", func(ctx context.Context, _ durable.Invocation) error {
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

const (
	reuseHandler      = "reuse"
	reuseOtherHandler = "reuse-other"
	reuseFailure      = "reuse failed"
)

type reuseLog struct {
	mu    sync.Mutex
	calls map[string][]string
}

func (l *reuseLog) add(key, entry string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls[key] = append(l.calls[key], entry)
}

func (l *reuseLog) of(key string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls[key])
}

func (l *reuseLog) handler(name string) durable.Handler {
	return func(ctx context.Context, inv durable.Invocation) error {
		key := inv.Run().Key
		payload := string(inv.Payload())
		l.add(key, name+":"+payload)
		switch payload {
		case "fail":
			return errors.New(reuseFailure)
		case "hold":
			<-ctx.Done()
			return ctx.Err()
		case "wait":
			signal, ok := inv.Signal(ctx, "wake")
			if !ok {
				return ctx.Err()
			}
			l.add(key, "signal:"+string(signal))
		}
		return nil
	}
}

type reuseSuite struct {
	backend durable.Runtime
	log     *reuseLog
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *reuseSuite) start(t *testing.T, req durable.StartRequest) error {
	t.Helper()
	ref, err := s.backend.Start(t.Context(), req)
	if err == nil && ref.Key != req.Key {
		t.Fatalf("Start(%q) ref = %+v", req.Key, ref)
	}
	return err
}

func (s *reuseSuite) mustStart(t *testing.T, req durable.StartRequest) {
	t.Helper()
	if err := s.start(t, req); err != nil {
		t.Fatalf("Start(%q, %q): %v", req.Handler, req.Key, err)
	}
}

func (s *reuseSuite) status(t *testing.T, key string) durable.RunStatus {
	t.Helper()
	status, err := s.backend.Status(t.Context(), durable.RunRef{Key: key})
	if err != nil {
		t.Fatalf("Status(%q): %v", key, err)
	}
	return status
}

func (s *reuseSuite) end(t *testing.T, key string, want durable.RunState) durable.RunStatus {
	t.Helper()
	waitForState(t.Context(), t, s.backend, key, want)
	return s.status(t, key)
}

func (s *reuseSuite) held(t *testing.T, key, payload string) {
	t.Helper()
	s.mustStart(t, durable.StartRequest{Handler: reuseHandler, Key: key, Payload: []byte(payload)})
	waitFor(t, "run "+key+" is invoked", func() bool { return len(s.log.of(key)) == 1 })
	waitForState(t.Context(), t, s.backend, key, durable.RunRunning)
}

func (s *reuseSuite) requireCalls(t *testing.T, key string, want ...string) {
	t.Helper()
	if got := s.log.of(key); !slices.Equal(got, want) {
		t.Fatalf("invocations of %q = %q, want %q", key, got, want)
	}
}

func (s *reuseSuite) settle(t *testing.T) {
	t.Helper()
	sentinel := "k-reuse-sentinel-" + t.Name()
	s.mustStart(t, durable.StartRequest{Handler: reuseHandler, Key: sentinel, Payload: []byte("ok")})
	s.end(t, sentinel, durable.RunSucceeded)
}

func ExerciseReuse(t *testing.T, backend durable.Runtime) {
	t.Helper()
	registrar, ok := backend.(durable.HandlerRegistrar)
	if !ok {
		t.Fatal("backend does not register handlers")
	}
	log := &reuseLog{calls: map[string][]string{}}
	registrar.RegisterHandler(reuseHandler, log.handler(reuseHandler))
	registrar.RegisterHandler(reuseOtherHandler, log.handler(reuseOtherHandler))
	s := &reuseSuite{backend: backend, log: log}

	t.Run("reuse_never_leaves_an_ended_run", s.neverLeavesAnEndedRun)
	t.Run("reuse_ended_starts_a_new_run", s.endedStartsANewRun)
	t.Run("reuse_ended_leaves_a_running_run", s.endedLeavesARunningRun)
	t.Run("reuse_ended_refuses_another_handlers_key", s.endedRefusesAnotherHandlersKey)
	t.Run("reuse_ended_needs_a_known_handler", s.endedNeedsAKnownHandler)
}

func (s *reuseSuite) neverLeavesAnEndedRun(t *testing.T) {
	key := "k-reuse-never"
	s.mustStart(t, durable.StartRequest{Handler: reuseHandler, Key: key, Payload: []byte("fail")})
	s.end(t, key, durable.RunFailed)
	s.mustStart(t, durable.StartRequest{Handler: reuseHandler, Key: key, Payload: []byte("ok")})
	if status := s.status(t, key); status.State != durable.RunFailed || status.Detail != reuseFailure {
		t.Fatalf("status after a second start = %+v, want the failed run", status)
	}
	s.settle(t)
	s.requireCalls(t, key, "reuse:fail")
	if status := s.status(t, key); status.State != durable.RunFailed || status.Detail != reuseFailure {
		t.Fatalf("status after the sentinel = %+v, want the failed run", status)
	}
}

func (s *reuseSuite) endedStartsANewRun(t *testing.T) {
	cases := []struct {
		name   string
		first  string
		ended  durable.RunState
		second string
		want   durable.RunState
	}{
		{name: "failed", first: "fail", ended: durable.RunFailed, second: "ok", want: durable.RunSucceeded},
		{name: "succeeded", first: "ok", ended: durable.RunSucceeded, second: "fail", want: durable.RunFailed},
		{name: "cancelled", first: "hold", ended: durable.RunCancelled, second: "ok", want: durable.RunSucceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := "k-reuse-ended-" + tc.name
			if tc.first == "hold" {
				s.held(t, key, tc.first)
				if err := s.backend.Cancel(t.Context(), durable.RunRef{Key: key}); err != nil {
					t.Fatalf("Cancel: %v", err)
				}
			} else {
				s.mustStart(t, durable.StartRequest{Handler: reuseHandler, Key: key, Payload: []byte(tc.first)})
			}
			s.end(t, key, tc.ended)
			s.mustStart(t, durable.StartRequest{Handler: reuseHandler, Key: key, Payload: []byte(tc.second), Reuse: durable.ReuseEnded})
			status := s.end(t, key, tc.want)
			s.requireCalls(t, key, "reuse:"+tc.first, "reuse:"+tc.second)
			if tc.want == durable.RunFailed && status.Detail != reuseFailure {
				t.Fatalf("detail = %q, want the new invocation's error", status.Detail)
			}
		})
	}
}

func (s *reuseSuite) endedLeavesARunningRun(t *testing.T) {
	key := "k-reuse-running"
	s.held(t, key, "wait")
	s.mustStart(t, durable.StartRequest{Handler: reuseHandler, Key: key, Payload: []byte("ok"), Reuse: durable.ReuseEnded})
	if status := s.status(t, key); status.State != durable.RunRunning {
		t.Fatalf("status after a reusing start = %+v, want running", status)
	}
	if err := s.backend.Signal(t.Context(), durable.RunRef{Key: key}, "wake", []byte("up")); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	s.end(t, key, durable.RunSucceeded)
	s.requireCalls(t, key, "reuse:wait", "signal:up")
}

func (s *reuseSuite) endedRefusesAnotherHandlersKey(t *testing.T) {
	ended := "k-reuse-other-ended"
	s.mustStart(t, durable.StartRequest{Handler: reuseHandler, Key: ended, Payload: []byte("fail")})
	s.end(t, ended, durable.RunFailed)
	running := "k-reuse-other-running"
	s.held(t, running, "hold")
	for _, key := range []string{ended, running} {
		before := s.status(t, key)
		req := durable.StartRequest{Handler: reuseOtherHandler, Key: key, Payload: []byte("ok"), Reuse: durable.ReuseEnded}
		if err := s.start(t, req); !errors.Is(err, durable.ErrKeyInUse) {
			t.Fatalf("reusing start of %q by another handler = %v, want ErrKeyInUse", key, err)
		}
		req.Reuse = durable.ReuseNever
		s.mustStart(t, req)
		if after := s.status(t, key); after != before {
			t.Fatalf("status of %q = %+v, want %+v", key, after, before)
		}
	}
	s.settle(t)
	s.requireCalls(t, ended, "reuse:fail")
	s.requireCalls(t, running, "reuse:hold")
	if err := s.backend.Cancel(t.Context(), durable.RunRef{Key: running}); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	s.end(t, running, durable.RunCancelled)
}

func (s *reuseSuite) endedNeedsAKnownHandler(t *testing.T) {
	key := "k-reuse-unknown"
	s.mustStart(t, durable.StartRequest{Handler: reuseHandler, Key: key, Payload: []byte("ok")})
	before := s.end(t, key, durable.RunSucceeded)
	err := s.start(t, durable.StartRequest{Handler: "reuse-missing", Key: key, Payload: []byte("fail"), Reuse: durable.ReuseEnded})
	if err == nil || errors.Is(err, durable.ErrKeyInUse) {
		t.Fatalf("reusing start with an unknown handler = %v, want the unknown handler error", err)
	}
	s.settle(t)
	if after := s.status(t, key); after != before {
		t.Fatalf("status = %+v, want %+v", after, before)
	}
	s.requireCalls(t, key, "reuse:ok")
}
