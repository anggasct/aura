package sqlite

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/durable/durabletest"
)

func TestRuntimePortConformance(t *testing.T) {
	rt := newRuntime(t, openDB(t, newDatabaseFile(t)), nil)
	startHost(t, rt)
	durabletest.ExerciseRuntime(t, rt)
}

func TestNewRejectsInvalidOptions(t *testing.T) {
	db := openDB(t, newDatabaseFile(t))
	cases := []struct {
		name string
		opts *Options
	}{
		{"nil options", nil},
		{"nil database", &Options{}},
		{"negative poll interval", &Options{DB: db, PollInterval: -time.Second}},
		{"negative host ttl", &Options{DB: db, HostTTL: -time.Second}},
		{"negative stop grace", &Options{DB: db, StopGrace: -time.Second}},
		{"negative recovery concurrency", &Options{DB: db, RecoveryConcurrency: -1}},
		{"poll interval below a millisecond", &Options{DB: db, PollInterval: time.Microsecond}},
		{"host ttl below a millisecond", &Options{DB: db, HostTTL: 999 * time.Microsecond}},
		{"stop grace below a millisecond", &Options{DB: db, StopGrace: time.Nanosecond}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt, err := New(tc.opts)
			if rt != nil {
				t.Fatal("New returned a runtime for invalid options")
			}
			requireCode(t, err, ErrorCodeInvalidArgument)
		})
	}
	rt, err := New(&Options{DB: db})
	if err != nil {
		t.Fatalf("New with defaults: %v", err)
	}
	if rt.pollInterval != defaultPollInterval || rt.hostTTL != defaultHostTTL || rt.stopGrace != defaultStopGrace || rt.recoveryConcurrency != defaultRecoveryConcurrency {
		t.Fatalf("defaults = %v %v %v %d", rt.pollInterval, rt.hostTTL, rt.stopGrace, rt.recoveryConcurrency)
	}
}

func TestRunRejectsSecondCall(t *testing.T) {
	rt := newRuntime(t, openDB(t, newDatabaseFile(t)), nil)
	h := startHost(t, rt)
	requireCode(t, rt.Run(t.Context()), ErrorCodeHostRunning)
	if err := h.stop(t); err != nil {
		t.Fatalf("stop: %v", err)
	}
	requireCode(t, rt.Run(t.Context()), ErrorCodeHostRunning)
}

func TestClientStartRunsOnTheHost(t *testing.T) {
	path := newDatabaseFile(t)
	server := newRuntime(t, openDB(t, path), nil)
	client := newRuntime(t, openDB(t, path), nil)
	var hostCalls, clientCalls atomic.Int64
	received := make(chan string, 1)
	server.RegisterHandler("job", func(ctx context.Context, inv durable.Invocation) error {
		hostCalls.Add(1)
		payload, ok := inv.Signal(ctx, "go")
		if !ok {
			return ctx.Err()
		}
		received <- string(payload)
		return nil
	})
	client.RegisterHandler("job", func(context.Context, durable.Invocation) error {
		clientCalls.Add(1)
		return nil
	})
	startHost(t, server)
	ref := mustStart(t, client, durable.StartRequest{Handler: "job", Key: "k-client"})
	mustSignal(t, client, sentSignal{ref: ref, name: "go", payload: "from-client"})
	waitState(t, client, runWant{key: ref.Key, state: durable.RunSucceeded})
	if got := receive(t, received); got != "from-client" {
		t.Fatalf("signal payload = %q", got)
	}
	if hostCalls.Load() != 1 || clientCalls.Load() != 0 {
		t.Fatalf("host calls %d, client calls %d; want 1 and 0", hostCalls.Load(), clientCalls.Load())
	}
}

func TestDeclaredHandlerStartsWithoutAFunction(t *testing.T) {
	path := newDatabaseFile(t)
	db := openDB(t, path)
	client := newRuntime(t, openDB(t, path), nil)
	client.DeclareHandlers("job", "lonely")
	_, err := client.Start(t.Context(), durable.StartRequest{Handler: "undeclared", Key: "k-undeclared"})
	requireCode(t, err, ErrorCodeHandlerNotRegistered)
	mustStart(t, client, durable.StartRequest{Handler: "lonely", Key: "k-lonely"})
	mustStart(t, client, durable.StartRequest{Handler: "job", Key: "k-declared"})
	server := newRuntime(t, db, nil)
	var calls invocationLog
	server.RegisterHandler("job", calls.handler())
	server.DeclareHandlers("lonely")
	startHost(t, server)
	waitState(t, client, runWant{key: "k-declared", state: durable.RunSucceeded})
	if got := calls.snapshot(); !slices.Equal(got, []string{"k-declared"}) {
		t.Fatalf("invoked = %v", got)
	}
	lonely := readRun(t, db, "k-lonely")
	if lonely.state != string(durable.RunRunning) || lonely.owner.Valid || lonely.claim != 0 {
		t.Fatalf("declared-only run = %+v, want running, unowned, never claimed", lonely)
	}
}

func TestClientCancelStopsTheHostsRun(t *testing.T) {
	path := newDatabaseFile(t)
	server := newRuntime(t, openDB(t, path), nil)
	client := newRuntime(t, openDB(t, path), nil)
	client.DeclareHandlers("hold")
	entered := make(chan struct{}, 1)
	server.RegisterHandler("hold", func(ctx context.Context, _ durable.Invocation) error {
		entered <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	})
	startHost(t, server)
	ref := mustStart(t, client, durable.StartRequest{Handler: "hold", Key: "k-hold"})
	receive(t, entered)
	if err := client.Cancel(t.Context(), ref); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	waitState(t, client, runWant{key: ref.Key, state: durable.RunCancelled})
}

func TestCancelWithoutHostNeverInvokes(t *testing.T) {
	path := newDatabaseFile(t)
	client := newRuntime(t, openDB(t, path), nil)
	client.DeclareHandlers("job")
	ref := mustStart(t, client, durable.StartRequest{Handler: "job", Key: "k-cancelled"})
	if err := client.Cancel(t.Context(), ref); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	waitState(t, client, runWant{key: ref.Key, state: durable.RunCancelled})
	server := newRuntime(t, openDB(t, path), nil)
	var calls invocationLog
	server.RegisterHandler("job", calls.handler())
	startHost(t, server)
	mustStart(t, client, durable.StartRequest{Handler: "job", Key: "k-sentinel"})
	waitState(t, client, runWant{key: "k-sentinel", state: durable.RunSucceeded})
	if got := calls.snapshot(); !slices.Equal(got, []string{"k-sentinel"}) {
		t.Fatalf("invoked = %v, want only the sentinel", got)
	}
	waitState(t, client, runWant{key: ref.Key, state: durable.RunCancelled})
}

func TestSignalToTerminalRunFails(t *testing.T) {
	rt := newRuntime(t, openDB(t, newDatabaseFile(t)), nil)
	rt.RegisterHandler("greet", func(context.Context, durable.Invocation) error { return nil })
	startHost(t, rt)
	ref := mustStart(t, rt, durable.StartRequest{Handler: "greet", Key: "k-done"})
	waitState(t, rt, runWant{key: ref.Key, state: durable.RunSucceeded})
	requireCode(t, rt.Signal(t.Context(), ref, "late", []byte("x")), ErrorCodeRunTerminal)
}

func TestHandlerPanicFailsTheRun(t *testing.T) {
	rt := newRuntime(t, openDB(t, newDatabaseFile(t)), nil)
	rt.RegisterHandler("boom", func(context.Context, durable.Invocation) error { panic("kaboom") })
	rt.RegisterHandler("calm", func(context.Context, durable.Invocation) error { return nil })
	startHost(t, rt)
	status := waitState(t, rt, runWant{key: mustStart(t, rt, durable.StartRequest{Handler: "boom", Key: "k-boom"}).Key, state: durable.RunFailed})
	if !strings.HasPrefix(status.Detail, "panic:") || !strings.Contains(status.Detail, "kaboom") {
		t.Fatalf("detail = %q, want the panic value", status.Detail)
	}
	waitState(t, rt, runWant{key: mustStart(t, rt, durable.StartRequest{Handler: "calm", Key: "k-calm"}).Key, state: durable.RunSucceeded})
}

func TestResolveApprovalIsConsumedOnce(t *testing.T) {
	db := openDB(t, newDatabaseFile(t))
	rt := newRuntime(t, db, nil)
	rt.DeclareHandlers("approval")
	mustStart(t, rt, durable.StartRequest{Handler: "approval", Key: "k-approval"})
	id := durable.FormatApprovalAddr("k-approval", "decision")
	if err := rt.ResolveApproval(t.Context(), durable.ResolveApprovalRequest{ApprovalID: id, Payload: []byte("yes")}); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	requireCode(t, rt.ResolveApproval(t.Context(), durable.ResolveApprovalRequest{ApprovalID: id}), ErrorCodeApprovalConsumed)
	if n := countRows(t, db, `SELECT COUNT(*) FROM durable_signal WHERE run_key = 'k-approval' AND name = 'decision'`); n != 1 {
		t.Fatalf("approval signals = %d, want 1", n)
	}
	later := durable.FormatApprovalAddr("k-later", "decision")
	if err := rt.ResolveApproval(t.Context(), durable.ResolveApprovalRequest{ApprovalID: later}); !errors.Is(err, durable.ErrUnknownRun) {
		t.Fatalf("resolve for an unknown run = %v, want ErrUnknownRun", err)
	}
	mustStart(t, rt, durable.StartRequest{Handler: "approval", Key: "k-later"})
	if err := rt.ResolveApproval(t.Context(), durable.ResolveApprovalRequest{ApprovalID: later}); err != nil {
		t.Fatalf("resolve once the run exists: %v", err)
	}
	for _, malformed := range []string{"", "no-separator"} {
		requireCode(t, rt.ResolveApproval(t.Context(), durable.ResolveApprovalRequest{ApprovalID: malformed}), ErrorCodeInvalidArgument)
	}
}

func TestCallRunsTheRegisteredHandler(t *testing.T) {
	rt := newRuntime(t, openDB(t, newDatabaseFile(t)), nil)
	rt.RegisterCall("sessions", "admit", func(_ context.Context, key string, payload []byte) ([]byte, error) {
		return []byte(key + ":" + string(payload)), nil
	})
	out, err := rt.Call(t.Context(), durable.CallRequest{Service: "sessions", Key: "s-1", Handler: "admit", Payload: []byte("turn")})
	if err != nil || string(out) != "s-1:turn" {
		t.Fatalf("Call = %q, %v", out, err)
	}
	_, err = rt.Call(t.Context(), durable.CallRequest{Service: "sessions", Key: "s-1", Handler: "release"})
	requireCode(t, err, ErrorCodeCallNotRegistered)
	_, err = rt.Call(t.Context(), durable.CallRequest{Service: "sessions", Handler: "admit"})
	requireCode(t, err, ErrorCodeInvalidArgument)
}

func TestTurnDeadlineIsUnsetUntilSessionsExist(t *testing.T) {
	rt := newRuntime(t, openDB(t, newDatabaseFile(t)), nil)
	deadline, ok, err := rt.TurnDeadline(t.Context(), "session-1")
	if err != nil || ok || !deadline.IsZero() {
		t.Fatalf("TurnDeadline = %v, %v, %v; want zero, false, nil", deadline, ok, err)
	}
	_, _, err = rt.TurnDeadline(t.Context(), "")
	requireCode(t, err, ErrorCodeInvalidArgument)
}
