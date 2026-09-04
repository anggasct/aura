package durable

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFakeStartIsIdempotentPerKey(t *testing.T) {
	fake := NewFake()
	var runs atomic.Int64
	fake.RegisterHandler("svc", func(_ context.Context, inv *Invocation) error {
		runs.Add(1)
		return nil
	})
	first, err := fake.Start(context.Background(), StartRequest{Handler: "svc", Key: "run-1"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	second, err := fake.Start(context.Background(), StartRequest{Handler: "svc", Key: "run-1"})
	if err != nil {
		t.Fatalf("Start again: %v", err)
	}
	if first != second {
		t.Fatalf("refs differ: %v vs %v", first, second)
	}
	fake.WaitReady(first)
	if got := runs.Load(); got != 1 {
		t.Fatalf("handler ran %d times, want 1", got)
	}
	status, err := fake.Status(context.Background(), first)
	if err != nil || status.State != RunSucceeded {
		t.Fatalf("status = %+v (%v), want succeeded", status, err)
	}
}

func TestFakeSignalQueuesBeforeAndWakesAfterWait(t *testing.T) {
	fake := NewFake()
	started := make(chan struct{})
	release := make(chan struct{})
	fake.RegisterHandler("svc", func(ctx context.Context, inv *Invocation) error {
		close(started)
		payload, ok := inv.Signal(ctx, "go")
		if !ok || string(payload) != "green" {
			t.Errorf("signal = %q, %v; want green", payload, ok)
		}
		close(release)
		return nil
	})
	run, err := fake.Start(context.Background(), StartRequest{Handler: "svc", Key: "run-1"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-started
	// Signal before the handler waits: the payload must queue.
	if err := fake.Signal(context.Background(), run, "go", []byte("green")); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	select {
	case <-release:
	case <-time.After(time.Second):
		t.Fatal("queued signal did not wake the handler")
	}
	fake.WaitReady(run)
}

func TestFakeCancelTerminatesWait(t *testing.T) {
	fake := NewFake()
	observed := make(chan error, 1)
	fake.RegisterHandler("svc", func(ctx context.Context, inv *Invocation) error {
		_, ok := inv.Signal(ctx, "never")
		observed <- map[bool]error{true: nil, false: context.Canceled}[ok]
		return nil
	})
	run, err := fake.Start(context.Background(), StartRequest{Handler: "svc", Key: "run-1"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := fake.Cancel(context.Background(), run); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if err := <-observed; !errIs(err, context.Canceled) {
		t.Fatalf("handler observed %v, want context.Canceled", err)
	}
	status, _ := fake.Status(context.Background(), run)
	if status.State != RunCancelled {
		t.Fatalf("state = %s, want cancelled", status.State)
	}
	if err := fake.Signal(context.Background(), run, "x", nil); err == nil {
		t.Fatal("signal accepted on a cancelled run")
	}
}

func errIs(err, target error) bool {
	return errors.Is(err, target)
}

func TestFakeSleepFiresOnManualClock(t *testing.T) {
	clock := NewManualClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	fake := NewFake().WithClock(clock)
	timerRegistered := make(chan struct{})
	slept := make(chan struct{})
	fake.RegisterHandler("svc", func(_ context.Context, inv *Invocation) error {
		timer := inv.Timer(time.Minute)
		close(timerRegistered)
		select {
		case <-timer:
		case <-inv.done:
			t.Errorf("sleep ended by cancellation")
		}
		close(slept)
		return nil
	})
	run, err := fake.Start(context.Background(), StartRequest{Handler: "svc", Key: "run-1"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-timerRegistered
	clock.Advance(time.Minute)
	select {
	case <-slept:
	case <-time.After(time.Second):
		t.Fatal("manual clock advance did not fire the timer")
	}
	fake.WaitReady(run)
}

func TestFakeUnknownRunOperationsFail(t *testing.T) {
	fake := NewFake()
	missing := RunRef{Key: "nope"}
	if _, err := fake.Status(context.Background(), missing); !errIs(err, ErrUnknownRun) {
		t.Fatalf("Status err = %v, want unknown run", err)
	}
	if err := fake.Signal(context.Background(), missing, "x", nil); !errIs(err, ErrUnknownRun) {
		t.Fatalf("Signal err = %v, want unknown run", err)
	}
	if err := fake.Cancel(context.Background(), missing); !errIs(err, ErrUnknownRun) {
		t.Fatalf("Cancel err = %v, want unknown run", err)
	}
}

func TestFakeFailureCarriesDetail(t *testing.T) {
	fake := NewFake()
	fake.RegisterHandler("svc", func(_ context.Context, inv *Invocation) error {
		return errors.New("step exploded")
	})
	run, err := fake.Start(context.Background(), StartRequest{Handler: "svc", Key: "run-1"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	fake.WaitReady(run)
	status, _ := fake.Status(context.Background(), run)
	if status.State != RunFailed || status.Detail != "step exploded" {
		t.Fatalf("status = %+v, want failed with detail", status)
	}
}

func TestFakeRunCompletionReleasesParkedSignal(t *testing.T) {
	fake := NewFake()
	parked := make(chan struct{})
	released := make(chan struct{})
	fake.RegisterHandler("svc", func(runCtx context.Context, inv *Invocation) error {
		go func() {
			close(parked)
			_, ok := inv.Signal(runCtx, "late")
			if ok {
				t.Error("parked signal returned a payload after the run finished")
			}
			close(released)
		}()
		<-parked
		return errors.New("step failed")
	})
	run, err := fake.Start(context.Background(), StartRequest{Handler: "svc", Key: "run-1"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	fake.WaitReady(run)
	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("parked Signal survived run completion")
	}
	if err := fake.Cancel(context.Background(), run); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
}

func TestSignalWaitCancellationPreservesRacedDelivery(t *testing.T) {
	inv := &Invocation{done: make(chan struct{}), mu: &sync.Mutex{}, signals: map[string]*signalQueue{}}
	ctx, cancel := context.WithCancel(context.Background())
	type signalResult struct {
		payload []byte
		ok      bool
	}
	result := make(chan signalResult, 1)
	go func() {
		payload, ok := inv.Signal(ctx, "x")
		result <- signalResult{payload: payload, ok: ok}
	}()
	registered := func() bool {
		inv.mu.Lock()
		defer inv.mu.Unlock()
		return inv.signals["x"] != nil && inv.signals["x"].waiter != nil
	}
	detached := func() bool {
		inv.mu.Lock()
		defer inv.mu.Unlock()
		return inv.signals["x"] == nil || inv.signals["x"].waiter == nil
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !registered() {
		time.Sleep(time.Millisecond)
	}
	cancel()
	for time.Now().Before(deadline) && !detached() {
		time.Sleep(time.Millisecond)
	}
	// Deliver after the abandoned wait detached itself: the payload must
	// queue for the next waiter instead of dying with the old attempt.
	inv.mu.Lock()
	queue := inv.signals["x"]
	if queue == nil {
		queue = &signalQueue{}
		inv.signals["x"] = queue
	}
	if queue.waiter != nil {
		queue.waiter <- signalDelivery{payload: []byte("raced")}
		queue.waiter = nil
	} else {
		queue.delivered = append(queue.delivered, signalDelivery{payload: []byte("raced")})
	}
	inv.mu.Unlock()
	select {
	case got := <-result:
		if got.ok {
			t.Fatalf("cancelled wait returned payload %q", got.payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled Signal never returned")
	}
	payload, ok := inv.Signal(context.Background(), "x")
	if !ok || string(payload) != "raced" {
		t.Fatalf("next Signal = %q, %v; want the raced delivery", payload, ok)
	}
}
