package sqlite

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/durable/durabletest"
)

type actionReport struct {
	out []byte
	err error
}

type waitReport struct {
	payload  string
	timedOut bool
	ok       bool
}

type restartPlan struct {
	path      string
	configure func(opts *Options)
	register  func(rt *Runtime)
}

func restart(t *testing.T, plan *restartPlan) *Runtime {
	t.Helper()
	rt := newRuntime(t, openDB(t, plan.path), plan.configure)
	plan.register(rt)
	startHost(t, rt)
	return rt
}

func parkUntilStopped(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestRestartReplaysRecordedActionsOnce(t *testing.T) {
	path := newDatabaseFile(t)
	var aCalls, bCalls atomic.Int64
	bRunning := make(chan struct{}, 1)
	handler := func(ctx context.Context, inv durable.Invocation) error {
		if _, err := inv.RunAction(ctx, "a", func(context.Context) ([]byte, error) {
			aCalls.Add(1)
			return []byte("A"), nil
		}); err != nil {
			return err
		}
		_, err := inv.RunAction(ctx, "b", func(ctx context.Context) ([]byte, error) {
			if bCalls.Add(1) == 1 {
				bRunning <- struct{}{}
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return []byte("B"), nil
		})
		return err
	}
	first := newRuntime(t, openDB(t, path), nil)
	first.RegisterHandler("replay", handler)
	h := startHost(t, first)
	ref := mustStart(t, first, durable.StartRequest{Handler: "replay", Key: "k-replay"})
	receive(t, bRunning)
	if err := h.stop(t); err != nil {
		t.Fatalf("stop: %v", err)
	}
	second := restart(t, &restartPlan{path: path, register: func(rt *Runtime) { rt.RegisterHandler("replay", handler) }})
	waitState(t, second, runWant{key: ref.Key, state: durable.RunSucceeded})
	if aCalls.Load() != 1 || bCalls.Load() != 2 {
		t.Fatalf("a ran %d times, b ran %d times; want 1 and 2", aCalls.Load(), bCalls.Load())
	}
}

func TestFailedActionIsRecorded(t *testing.T) {
	path := newDatabaseFile(t)
	refused := errors.New("provider refused the charge")
	var calls atomic.Int64
	charge := func(context.Context) ([]byte, error) {
		calls.Add(1)
		return nil, refused
	}
	reports := make(chan error, 3)
	first := newRuntime(t, openDB(t, path), nil)
	first.RegisterHandler("charge", func(ctx context.Context, inv durable.Invocation) error {
		_, err := inv.RunAction(ctx, "charge", charge)
		reports <- err
		_, err = inv.RunAction(ctx, "charge", charge)
		reports <- err
		return parkUntilStopped(ctx)
	})
	h := startHost(t, first)
	ref := mustStart(t, first, durable.StartRequest{Handler: "charge", Key: "k-charge"})
	firstErr := receive(t, reports)
	if detail := requireCode(t, firstErr, ErrorCodeActionFailed); detail != refused.Error() {
		t.Fatalf("detail = %q", detail)
	}
	if !errors.Is(firstErr, refused) {
		t.Fatalf("first failure %v does not reach the original error", firstErr)
	}
	if detail := requireCode(t, receive(t, reports), ErrorCodeActionFailed); detail != refused.Error() {
		t.Fatalf("repeated detail = %q", detail)
	}
	if err := h.stop(t); err != nil {
		t.Fatalf("stop: %v", err)
	}
	second := restart(t, &restartPlan{path: path, register: func(rt *Runtime) {
		rt.RegisterHandler("charge", func(ctx context.Context, inv durable.Invocation) error {
			_, err := inv.RunAction(ctx, "charge", charge)
			reports <- err
			return nil
		})
	}})
	if detail := requireCode(t, receive(t, reports), ErrorCodeActionFailed); detail != refused.Error() {
		t.Fatalf("replayed detail = %q", detail)
	}
	waitState(t, second, runWant{key: ref.Key, state: durable.RunSucceeded})
	if calls.Load() != 1 {
		t.Fatalf("action function ran %d times, want 1", calls.Load())
	}
}

func TestActionResultIsRecordedWhenItsContextExpires(t *testing.T) {
	path := newDatabaseFile(t)
	var calls atomic.Int64
	slow := func(ctx context.Context) ([]byte, error) {
		calls.Add(1)
		<-ctx.Done()
		return []byte("late"), nil
	}
	reports := make(chan actionReport, 2)
	first := newRuntime(t, openDB(t, path), nil)
	first.RegisterHandler("slow", func(ctx context.Context, inv durable.Invocation) error {
		actionCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		out, err := inv.RunAction(actionCtx, "slow", slow)
		reports <- actionReport{out: out, err: err}
		return parkUntilStopped(ctx)
	})
	h := startHost(t, first)
	ref := mustStart(t, first, durable.StartRequest{Handler: "slow", Key: "k-slow"})
	if got := receive(t, reports); got.err != nil || string(got.out) != "late" {
		t.Fatalf("RunAction = %q, %v", got.out, got.err)
	}
	if err := h.stop(t); err != nil {
		t.Fatalf("stop: %v", err)
	}
	second := restart(t, &restartPlan{path: path, register: func(rt *Runtime) {
		rt.RegisterHandler("slow", func(ctx context.Context, inv durable.Invocation) error {
			out, err := inv.RunAction(ctx, "slow", slow)
			reports <- actionReport{out: out, err: err}
			return err
		})
	}})
	if got := receive(t, reports); got.err != nil || string(got.out) != "late" {
		t.Fatalf("replayed RunAction = %q, %v", got.out, got.err)
	}
	waitState(t, second, runWant{key: ref.Key, state: durable.RunSucceeded})
	if calls.Load() != 1 {
		t.Fatalf("action function ran %d times, want 1", calls.Load())
	}
}

func TestRacingActionsOnOneKeyShareOneResult(t *testing.T) {
	db := openDB(t, newDatabaseFile(t))
	rt := newRuntime(t, db, nil)
	reports := make(chan []actionReport, 1)
	rt.RegisterHandler("race", func(ctx context.Context, inv durable.Invocation) error {
		var entered atomic.Int64
		both := make(chan struct{})
		racer := func(value string) func(context.Context) ([]byte, error) {
			return func(ctx context.Context) ([]byte, error) {
				if entered.Add(1) == 2 {
					close(both)
				}
				select {
				case <-both:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return []byte(value), nil
			}
		}
		results := make([]actionReport, 2)
		var wg sync.WaitGroup
		for i, value := range []string{"one", "two"} {
			wg.Go(func() {
				out, err := inv.RunAction(ctx, "shared", racer(value))
				results[i] = actionReport{out: out, err: err}
			})
		}
		wg.Wait()
		reports <- results
		return nil
	})
	startHost(t, rt)
	ref := mustStart(t, rt, durable.StartRequest{Handler: "race", Key: "k-race"})
	results := receive(t, reports)
	for _, result := range results {
		if result.err != nil {
			t.Fatalf("RunAction: %v", result.err)
		}
	}
	if !bytes.Equal(results[0].out, results[1].out) {
		t.Fatalf("racing results differ: %q vs %q", results[0].out, results[1].out)
	}
	if got := string(results[0].out); got != "one" && got != "two" {
		t.Fatalf("shared result = %q", got)
	}
	waitState(t, rt, runWant{key: ref.Key, state: durable.RunSucceeded})
	if n := countRows(t, db, `SELECT COUNT(*) FROM durable_action WHERE run_key = 'k-race'`); n != 1 {
		t.Fatalf("action rows = %d, want 1", n)
	}
}

func TestRestartKeepsSignalOrder(t *testing.T) {
	path := newDatabaseFile(t)
	reads := make(chan string, 3)
	first := newRuntime(t, openDB(t, path), nil)
	first.RegisterHandler("reader", func(ctx context.Context, inv durable.Invocation) error {
		payload, ok := inv.Signal(ctx, "item")
		if !ok {
			return ctx.Err()
		}
		reads <- string(payload)
		return parkUntilStopped(ctx)
	})
	ref := mustStart(t, first, durable.StartRequest{Handler: "reader", Key: "k-reader"})
	mustSignal(t, first, sentSignal{ref: ref, name: "item", payload: "first"})
	mustSignal(t, first, sentSignal{ref: ref, name: "item", payload: "second"})
	h := startHost(t, first)
	if got := receive(t, reads); got != "first" {
		t.Fatalf("first read = %q", got)
	}
	if err := h.stop(t); err != nil {
		t.Fatalf("stop: %v", err)
	}
	second := restart(t, &restartPlan{path: path, register: func(rt *Runtime) {
		rt.RegisterHandler("reader", func(ctx context.Context, inv durable.Invocation) error {
			for range 2 {
				payload, ok := inv.Signal(ctx, "item")
				if !ok {
					return ctx.Err()
				}
				reads <- string(payload)
			}
			return nil
		})
	}})
	if a, b := receive(t, reads), receive(t, reads); a != "first" || b != "second" {
		t.Fatalf("replayed reads = %q, %q; want first, second", a, b)
	}
	waitState(t, second, runWant{key: ref.Key, state: durable.RunSucceeded})
}

func TestRestartKeepsWaitTimeout(t *testing.T) {
	path := newDatabaseFile(t)
	reports := make(chan waitReport, 2)
	first := newRuntime(t, openDB(t, path), nil)
	first.RegisterHandler("waiter", func(ctx context.Context, inv durable.Invocation) error {
		payload, timedOut, ok := inv.Wait(ctx, "answer", 20*time.Millisecond)
		reports <- waitReport{payload: string(payload), timedOut: timedOut, ok: ok}
		return parkUntilStopped(ctx)
	})
	h := startHost(t, first)
	ref := mustStart(t, first, durable.StartRequest{Handler: "waiter", Key: "k-waiter"})
	if got := receive(t, reports); !got.ok || !got.timedOut {
		t.Fatalf("first wait = %+v, want a timeout", got)
	}
	mustSignal(t, first, sentSignal{ref: ref, name: "answer", payload: "late"})
	if err := h.stop(t); err != nil {
		t.Fatalf("stop: %v", err)
	}
	second := restart(t, &restartPlan{path: path, register: func(rt *Runtime) {
		rt.RegisterHandler("waiter", func(ctx context.Context, inv durable.Invocation) error {
			payload, timedOut, ok := inv.Wait(ctx, "answer", time.Hour)
			reports <- waitReport{payload: string(payload), timedOut: timedOut, ok: ok}
			late, ok := inv.Signal(ctx, "answer")
			reports <- waitReport{payload: string(late), ok: ok}
			return nil
		})
	}})
	if got := receive(t, reports); !got.ok || !got.timedOut {
		t.Fatalf("replayed wait = %+v, want the recorded timeout", got)
	}
	if got := receive(t, reports); !got.ok || got.payload != "late" {
		t.Fatalf("next signal = %+v, want the late signal", got)
	}
	waitState(t, second, runWant{key: ref.Key, state: durable.RunSucceeded})
}

func TestRestartKeepsWaitDeadline(t *testing.T) {
	path := newDatabaseFile(t)
	clock := durabletest.NewManualClock(time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC))
	withClock := func(opts *Options) { opts.Clock = clock }
	reports := make(chan waitReport, 1)
	waiter := func(ctx context.Context, inv durable.Invocation) error {
		payload, timedOut, ok := inv.Wait(ctx, "approval", time.Hour)
		if !ok {
			return ctx.Err()
		}
		reports <- waitReport{payload: string(payload), timedOut: timedOut, ok: ok}
		return nil
	}
	first := newRuntime(t, openDB(t, path), withClock)
	first.RegisterHandler("waiter", waiter)
	h := startHost(t, first)
	ref := mustStart(t, first, durable.StartRequest{Handler: "waiter", Key: "k-deadline"})
	waitUntil(t, "the wait parks", func() bool { return first.stats().parked == 1 })
	clock.Advance(40 * time.Minute)
	if err := h.stop(t); err != nil {
		t.Fatalf("stop: %v", err)
	}
	second := restart(t, &restartPlan{path: path, configure: withClock, register: func(rt *Runtime) { rt.RegisterHandler("waiter", waiter) }})
	waitUntil(t, "the resumed wait parks", func() bool { return second.stats().parked == 1 })
	clock.Advance(20 * time.Minute)
	if got := receive(t, reports); !got.ok || !got.timedOut {
		t.Fatalf("resumed wait = %+v, want a timeout at the recorded deadline", got)
	}
	waitState(t, second, runWant{key: ref.Key, state: durable.RunSucceeded})
}

func TestZeroTimeoutWaitSeesALaterSignal(t *testing.T) {
	rt := newRuntime(t, openDB(t, newDatabaseFile(t)), nil)
	reports := make(chan waitReport, 2)
	proceed := make(chan struct{})
	rt.RegisterHandler("poller", func(ctx context.Context, inv durable.Invocation) error {
		payload, timedOut, ok := inv.Wait(ctx, "cancel", 0)
		reports <- waitReport{payload: string(payload), timedOut: timedOut, ok: ok}
		select {
		case <-proceed:
		case <-ctx.Done():
			return ctx.Err()
		}
		payload, timedOut, ok = inv.Wait(ctx, "cancel", 0)
		reports <- waitReport{payload: string(payload), timedOut: timedOut, ok: ok}
		return nil
	})
	startHost(t, rt)
	ref := mustStart(t, rt, durable.StartRequest{Handler: "poller", Key: "k-poller"})
	if got := receive(t, reports); !got.ok || !got.timedOut {
		t.Fatalf("first poll = %+v, want a timeout", got)
	}
	mustSignal(t, rt, sentSignal{ref: ref, name: "cancel", payload: "stop"})
	close(proceed)
	if got := receive(t, reports); !got.ok || got.timedOut || got.payload != "stop" {
		t.Fatalf("second poll = %+v, want the signal", got)
	}
	waitState(t, rt, runWant{key: ref.Key, state: durable.RunSucceeded})
}

func TestRestartKeepsTimerFireTime(t *testing.T) {
	path := newDatabaseFile(t)
	clock := durabletest.NewManualClock(time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC))
	withClock := func(opts *Options) { opts.Clock = clock }
	sleeper := func(_ context.Context, inv durable.Invocation) error {
		return inv.Sleep(time.Hour)
	}
	alarm := func(ctx context.Context, inv durable.Invocation) error {
		select {
		case <-inv.Timer(time.Hour):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	register := func(rt *Runtime) {
		rt.RegisterHandler("sleeper", sleeper)
		rt.RegisterHandler("alarm", alarm)
	}

	first := newRuntime(t, openDB(t, path), withClock)
	register(first)
	h := startHost(t, first)
	sleep := mustStart(t, first, durable.StartRequest{Handler: "sleeper", Key: "k-sleep"})
	waitUntil(t, "the sleep parks", func() bool { return first.stats().parked == 1 })
	clock.Advance(40 * time.Minute)
	if err := h.stop(t); err != nil {
		t.Fatalf("stop: %v", err)
	}
	second := newRuntime(t, openDB(t, path), withClock)
	register(second)
	h = startHost(t, second)
	waitUntil(t, "the resumed sleep parks", func() bool { return second.stats().parked == 1 })
	clock.Advance(20 * time.Minute)
	waitState(t, second, runWant{key: sleep.Key, state: durable.RunSucceeded})

	db := openDB(t, path)
	timer := mustStart(t, second, durable.StartRequest{Handler: "alarm", Key: "k-alarm"})
	waitUntil(t, "the timer is recorded", func() bool {
		return countRows(t, db, `SELECT COUNT(*) FROM durable_timer WHERE run_key = 'k-alarm'`) == 1
	})
	if err := h.stop(t); err != nil {
		t.Fatalf("stop: %v", err)
	}
	clock.Advance(2 * time.Hour)
	third := restart(t, &restartPlan{path: path, configure: withClock, register: register})
	waitState(t, third, runWant{key: timer.Key, state: durable.RunSucceeded})
}

type suspendedClock struct {
	timers *durabletest.ManualClock
	mu     sync.Mutex
	jumped time.Duration
}

func (c *suspendedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.timers.Now().Add(c.jumped)
}

func (c *suspendedClock) Timer(d time.Duration) <-chan time.Time {
	return c.timers.Timer(d)
}

func (c *suspendedClock) jump(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.jumped += d
}

func TestSleepFollowsTheWallClock(t *testing.T) {
	clock := &suspendedClock{timers: durabletest.NewManualClock(time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC))}
	const check = 30 * time.Second
	rt := newRuntime(t, openDB(t, newDatabaseFile(t)), func(opts *Options) {
		opts.Clock = clock
		opts.ClockCheck = check
	})
	reports := make(chan waitReport, 1)
	rt.RegisterHandler("sleeper", func(_ context.Context, inv durable.Invocation) error {
		return inv.Sleep(time.Hour)
	})
	rt.RegisterHandler("waiter", func(ctx context.Context, inv durable.Invocation) error {
		payload, timedOut, ok := inv.Wait(ctx, "never", time.Hour)
		reports <- waitReport{payload: string(payload), timedOut: timedOut, ok: ok}
		return nil
	})
	startHost(t, rt)

	sleep := mustStart(t, rt, durable.StartRequest{Handler: "sleeper", Key: "k-suspended-sleep"})
	waitUntil(t, "the sleep parks", func() bool { return rt.stats().parked == 1 })
	clock.jump(2 * time.Hour)
	clock.timers.Advance(check)
	waitState(t, rt, runWant{key: sleep.Key, state: durable.RunSucceeded})

	wait := mustStart(t, rt, durable.StartRequest{Handler: "waiter", Key: "k-suspended-wait"})
	waitUntil(t, "the wait parks", func() bool { return rt.stats().parked == 1 })
	clock.jump(2 * time.Hour)
	clock.timers.Advance(check)
	if got := receive(t, reports); !got.ok || !got.timedOut {
		t.Fatalf("wait = %+v, want a timeout at the wall-clock deadline", got)
	}
	waitState(t, rt, runWant{key: wait.Key, state: durable.RunSucceeded})
}
