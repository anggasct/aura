package durable

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFakeStartIsIdempotentPerKey(t *testing.T) {
	fake := NewFake()
	var runs atomic.Int64
	fake.RegisterHandler("svc", func(_ context.Context, inv Invocation) error {
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
	fake.RegisterHandler("svc", func(ctx context.Context, inv Invocation) error {
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
	fake.RegisterHandler("svc", func(ctx context.Context, inv Invocation) error {
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
	fake.RegisterHandler("svc", func(ctx context.Context, inv Invocation) error {
		timer := inv.Timer(time.Minute)
		close(timerRegistered)
		select {
		case <-timer:
		case <-ctx.Done():
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
	fake.RegisterHandler("svc", func(_ context.Context, inv Invocation) error {
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
	fake.RegisterHandler("svc", func(runCtx context.Context, inv Invocation) error {
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
	inv := &invocation{run: RunRef{Key: "test"}, clock: RealClock(), done: make(chan struct{}), mu: &sync.Mutex{}, signals: map[string]*signalQueue{}}
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

func TestFakeCallDispatchesByServiceAndHandler(t *testing.T) {
	fake := NewFake()
	fake.RegisterCall("SessionTurns", "admit", func(_ context.Context, key string, payload []byte) ([]byte, error) {
		return []byte(`{"key":` + strconv.Quote(key) + `,"payload":` + string(payload) + `}`), nil
	})
	out, err := fake.Call(context.Background(), CallRequest{
		Service: "SessionTurns",
		Key:     "session-1",
		Handler: "admit",
		Payload: []byte(`{"turn":"turn-1"}`),
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	want := `{"key":"session-1","payload":{"turn":"turn-1"}}`
	if string(out) != want {
		t.Fatalf("response = %s, want %s", out, want)
	}
}

func TestFakeCallRejectsUnknownAndInvalid(t *testing.T) {
	fake := NewFake()
	cases := map[string]CallRequest{
		"unknown handler": {Service: "SessionTurns", Key: "s", Handler: "nope", Payload: []byte(`{}`)},
		"missing service": {Key: "s", Handler: "admit"},
		"missing key":     {Service: "SessionTurns", Handler: "admit"},
		"missing handler": {Service: "SessionTurns", Key: "s"},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := fake.Call(context.Background(), req); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestFakeResolveApprovalConsumesOnce(t *testing.T) {
	fake := NewFake()
	fake.RegisterHandler("svc", func(ctx context.Context, inv Invocation) error {
		_, _, _ = inv.Wait(ctx, "approval-op-1", time.Minute)
		return nil
	})
	ref, err := fake.Start(context.Background(), StartRequest{Handler: "svc", Key: "turn-1"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_ = ref
	addr := FormatApprovalAddr("turn-1", "approval-op-1")
	if err := fake.ResolveApproval(context.Background(), ResolveApprovalRequest{ApprovalID: addr, Payload: []byte(`{"decision":"approve"}`)}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := fake.ResolveApproval(context.Background(), ResolveApprovalRequest{ApprovalID: addr, Payload: []byte(`{"decision":"approve"}`)}); err == nil {
		t.Fatal("expected a double-consume error")
	}
	if _, _, err := fake.TurnDeadline(context.Background(), "session-1"); err != nil {
		t.Fatalf("deadline lookup: %v", err)
	}
	when := time.Now().Add(time.Hour).Truncate(time.Second)
	fake.RegisterTurnDeadline("session-1", when)
	deadline, ok, err := fake.TurnDeadline(context.Background(), "session-1")
	if err != nil || !ok || !deadline.Equal(when) {
		t.Fatalf("deadline = %v %v (%v), want %v true", deadline, ok, err, when)
	}
	if err := fake.ResolveApproval(context.Background(), ResolveApprovalRequest{}); err == nil {
		t.Fatal("expected an empty-id error")
	}
	if _, _, err := fake.TurnDeadline(context.Background(), ""); err == nil {
		t.Fatal("expected an empty-session error")
	}
}

func TestParseApprovalAddr(t *testing.T) {
	runKey, signal, err := ParseApprovalAddr("turn-1/approval-op-2")
	if err != nil || runKey != "turn-1" || signal != "approval-op-2" {
		t.Fatalf("parsed = %q %q (%v)", runKey, signal, err)
	}
	for _, bad := range []string{"", "noslash", "/signal", "run/"} {
		if _, _, err := ParseApprovalAddr(bad); err == nil {
			t.Fatalf("expected an error for %q", bad)
		}
	}
}
