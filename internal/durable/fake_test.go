package durable

import (
	"context"
	"errors"
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
	fake.RegisterHandler("svc", func(_ context.Context, inv *Invocation) error {
		close(started)
		payload, ok := inv.Signal("go")
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
	fake.RegisterHandler("svc", func(_ context.Context, inv *Invocation) error {
		_, ok := inv.Signal("never")
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
