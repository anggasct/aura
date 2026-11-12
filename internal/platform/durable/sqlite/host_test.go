package sqlite

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
)

func TestHostStopLeavesRunRunning(t *testing.T) {
	path := newDatabaseFile(t)
	db := openDB(t, path)
	first := newRuntime(t, openDB(t, path), nil)
	first.RegisterHandler("parked", func(ctx context.Context, inv durable.Invocation) error {
		if _, ok := inv.Signal(ctx, "go"); !ok {
			return ctx.Err()
		}
		return nil
	})
	h := startHost(t, first)
	ref := mustStart(t, first, durable.StartRequest{Handler: "parked", Key: "k-parked"})
	waitUntil(t, "the handler parks", func() bool { return first.stats().parked == 1 })
	if err := h.stop(t); err != nil {
		t.Fatalf("stop: %v", err)
	}
	row := readRun(t, db, ref.Key)
	if row.state != string(durable.RunRunning) || row.owner.Valid || row.detail != "" {
		t.Fatalf("stopped run = %+v, want running with no owner", row)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM durable_host`); n != 0 {
		t.Fatalf("host rows after stop = %d, want 0", n)
	}
	second := restart(t, &restartPlan{path: path, register: func(rt *Runtime) {
		rt.RegisterHandler("parked", func(ctx context.Context, inv durable.Invocation) error {
			if _, ok := inv.Signal(ctx, "go"); !ok {
				return ctx.Err()
			}
			return nil
		})
	}})
	mustSignal(t, second, sentSignal{ref: ref, name: "go", payload: ""})
	waitState(t, second, runWant{key: ref.Key, state: durable.RunSucceeded})
}

func TestInvocationContextCarriesItsCause(t *testing.T) {
	path := newDatabaseFile(t)
	entered := make(chan struct{}, 2)
	causes := make(chan error, 2)
	handler := func(ctx context.Context, _ durable.Invocation) error {
		entered <- struct{}{}
		<-ctx.Done()
		causes <- context.Cause(ctx)
		return ctx.Err()
	}
	first := newRuntime(t, openDB(t, path), nil)
	first.RegisterHandler("watch", handler)
	h := startHost(t, first)
	ref := mustStart(t, first, durable.StartRequest{Handler: "watch", Key: "k-watch"})
	receive(t, entered)
	if err := h.stop(t); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if cause := receive(t, causes); !errors.Is(cause, durable.ErrHostStopping) {
		t.Fatalf("cause after stop = %v, want ErrHostStopping", cause)
	}
	second := restart(t, &restartPlan{path: path, register: func(rt *Runtime) { rt.RegisterHandler("watch", handler) }})
	receive(t, entered)
	if err := second.Cancel(t.Context(), ref); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if cause := receive(t, causes); !errors.Is(cause, durable.ErrRunCancelled) {
		t.Fatalf("cause after cancel = %v, want ErrRunCancelled", cause)
	}
	waitState(t, second, runWant{key: ref.Key, state: durable.RunCancelled})
}

func TestStopGraceReleasesRunsOfAStuckHandler(t *testing.T) {
	db := openDB(t, newDatabaseFile(t))
	rt := newRuntime(t, db, func(opts *Options) { opts.StopGrace = 50 * time.Millisecond })
	entered := make(chan struct{}, 1)
	stuck := make(chan struct{})
	rt.RegisterHandler("stuck", func(context.Context, durable.Invocation) error {
		entered <- struct{}{}
		<-stuck
		return nil
	})
	h := startHost(t, rt)
	ref := mustStart(t, rt, durable.StartRequest{Handler: "stuck", Key: "k-stuck"})
	receive(t, entered)
	requireCode(t, h.stop(t), ErrorCodeStopTimeout)
	released := readRun(t, db, ref.Key)
	if released.state != string(durable.RunRunning) || released.owner.Valid {
		t.Fatalf("run after stop = %+v, want running with no owner", released)
	}
	close(stuck)
	waitUntil(t, "the stuck handler ends", func() bool { return rt.stats().ended == 1 })
	if after := readRun(t, db, ref.Key); after != released {
		t.Fatalf("late handler changed the row: %+v, want %+v", after, released)
	}
}

func TestClaimedRunWithCancelRequestIsNotInvoked(t *testing.T) {
	path := newDatabaseFile(t)
	db := openDB(t, path)
	insertHostRow(t, db, hostFixture{owner: "other-host", expiresInMS: time.Hour.Milliseconds()})
	insertRun(t, db, runFixture{key: "k-requested", handler: "job", owner: "other-host", claim: 1})
	client := newRuntime(t, openDB(t, path), nil)
	client.DeclareHandlers("job")
	if err := client.Cancel(t.Context(), durable.RunRef{Key: "k-requested"}); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if row := readRun(t, db, "k-requested"); row.state != string(durable.RunRunning) || !row.cancelRequested {
		t.Fatalf("run after cancel request = %+v, want running with the request recorded", row)
	}
	execSQL(t, db, `UPDATE durable_host SET expires_at = `+dbNowMS+` - 1000 WHERE owner = 'other-host'`)
	mustStart(t, client, durable.StartRequest{Handler: "job", Key: "k-sentinel"})
	server := newRuntime(t, openDB(t, path), nil)
	var calls invocationLog
	server.RegisterHandler("job", calls.handler())
	startHost(t, server)
	waitState(t, client, runWant{key: "k-sentinel", state: durable.RunSucceeded})
	waitState(t, client, runWant{key: "k-requested", state: durable.RunCancelled})
	if got := calls.snapshot(); !slices.Equal(got, []string{"k-sentinel"}) {
		t.Fatalf("invoked = %v, want only the sentinel", got)
	}
}

func TestExpiredHostsRunsAreClaimed(t *testing.T) {
	db := openDB(t, newDatabaseFile(t))
	insertHostRow(t, db, hostFixture{owner: "dead-host", expiresInMS: -1000})
	insertRun(t, db, runFixture{key: "k-orphan", handler: "job", owner: "dead-host", claim: 3})
	rt := newRuntime(t, db, nil)
	var calls invocationLog
	rt.RegisterHandler("job", calls.handler())
	startHost(t, rt)
	waitState(t, rt, runWant{key: "k-orphan", state: durable.RunSucceeded})
	if got := calls.snapshot(); !slices.Equal(got, []string{"k-orphan"}) {
		t.Fatalf("invoked = %v", got)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM durable_host WHERE owner = 'dead-host'`); n != 0 {
		t.Fatalf("expired host rows = %d, want 0", n)
	}
	if row := readRun(t, db, "k-orphan"); row.claim != 4 {
		t.Fatalf("claim = %d, want 4", row.claim)
	}
}

func TestLiveHostsRunsAreNotClaimed(t *testing.T) {
	db := openDB(t, newDatabaseFile(t))
	insertHostRow(t, db, hostFixture{owner: "live-host", expiresInMS: time.Hour.Milliseconds()})
	insertRun(t, db, runFixture{key: "k-owned", handler: "job", owner: "live-host", claim: 1})
	insertRun(t, db, runFixture{key: "k-free", handler: "job"})
	rt := newRuntime(t, db, nil)
	var calls invocationLog
	rt.RegisterHandler("job", calls.handler())
	startHost(t, rt)
	waitState(t, rt, runWant{key: "k-free", state: durable.RunSucceeded})
	if got := calls.snapshot(); !slices.Equal(got, []string{"k-free"}) {
		t.Fatalf("invoked = %v, want only the unowned run", got)
	}
	row := readRun(t, db, "k-owned")
	if row.state != string(durable.RunRunning) || row.owner.String != "live-host" || row.claim != 1 {
		t.Fatalf("live host's run = %+v", row)
	}
}

func TestLeaseRenewalKeepsRunsOwned(t *testing.T) {
	path := newDatabaseFile(t)
	db := openDB(t, path)
	var parkedCalls atomic.Int64
	entered := make(chan struct{}, 2)
	parked := func(ctx context.Context, _ durable.Invocation) error {
		parkedCalls.Add(1)
		entered <- struct{}{}
		return parkUntilStopped(ctx)
	}
	first := newRuntime(t, openDB(t, path), func(opts *Options) { opts.HostTTL = 600 * time.Millisecond })
	first.RegisterHandler("parked", parked)
	startHost(t, first)
	ref := mustStart(t, first, durable.StartRequest{Handler: "parked", Key: "k-parked"})
	receive(t, entered)
	second := newRuntime(t, openDB(t, path), nil)
	second.RegisterHandler("parked", parked)
	var sentinel invocationLog
	second.RegisterHandler("sentinel", sentinel.handler())
	startHost(t, second)
	mustStart(t, second, durable.StartRequest{Handler: "sentinel", Key: "k-sentinel"})
	waitState(t, second, runWant{key: "k-sentinel", state: durable.RunSucceeded})
	waitUntil(t, "the lease renews three times", func() bool { return first.stats().renewals >= 3 })
	if parkedCalls.Load() != 1 {
		t.Fatalf("parked handler ran %d times, want 1", parkedCalls.Load())
	}
	if row := readRun(t, db, ref.Key); row.owner.String != first.hostID || row.claim != 1 {
		t.Fatalf("parked run = %+v, want owned by the first host with claim 1", row)
	}
}

func TestLostOwnershipStopsJournalWrites(t *testing.T) {
	db := openDB(t, newDatabaseFile(t))
	rt := newRuntime(t, db, nil)
	between := make(chan struct{}, 1)
	proceed := make(chan struct{})
	type lostReport struct {
		err   error
		cause error
	}
	reports := make(chan lostReport, 1)
	var bCalls atomic.Int64
	rt.RegisterHandler("journal", func(ctx context.Context, inv durable.Invocation) error {
		if _, err := inv.RunAction(ctx, "a", func(context.Context) ([]byte, error) { return []byte("A"), nil }); err != nil {
			return err
		}
		between <- struct{}{}
		<-proceed
		_, err := inv.RunAction(ctx, "b", func(context.Context) ([]byte, error) {
			bCalls.Add(1)
			return []byte("B"), nil
		})
		reports <- lostReport{err: err, cause: context.Cause(ctx)}
		return err
	})
	startHost(t, rt)
	ref := mustStart(t, rt, durable.StartRequest{Handler: "journal", Key: "k-journal"})
	receive(t, between)
	insertHostRow(t, db, hostFixture{owner: "usurper", expiresInMS: time.Hour.Milliseconds()})
	execSQL(t, db, `UPDATE durable_run SET owner = 'usurper', claim = 7 WHERE key = ?`, ref.Key)
	close(proceed)
	report := receive(t, reports)
	requireCode(t, report.err, ErrorCodeOwnershipLost)
	if !errors.Is(report.cause, durable.ErrOwnershipLost) {
		t.Fatalf("cause = %v, want ErrOwnershipLost", report.cause)
	}
	if bCalls.Load() != 1 {
		t.Fatalf("second action ran %d times, want 1", bCalls.Load())
	}
	waitUntil(t, "the handler ends", func() bool { return rt.stats().ended == 1 })
	row := readRun(t, db, ref.Key)
	if row.state != string(durable.RunRunning) || row.owner.String != "usurper" || row.claim != 7 {
		t.Fatalf("run after lost ownership = %+v", row)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM durable_action WHERE run_key = ? AND action_key = 'b'`, ref.Key); n != 0 {
		t.Fatalf("fenced action rows = %d, want 0", n)
	}
}

func TestRecoveryIsBounded(t *testing.T) {
	db := openDB(t, newDatabaseFile(t))
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	keys := make([]string, 0, 5)
	for i := range 5 {
		key := fmt.Sprintf("k-replay-%d", i)
		keys = append(keys, key)
		insertRun(t, db, runFixture{key: key, handler: "replay"})
		execSQL(t, db, `INSERT INTO durable_action (run_key, action_key, outcome, result, detail, created_at) VALUES (?, 'seen', 'result', ?, '', ?)`, key, []byte("x"), stamp)
	}
	rt := newRuntime(t, db, func(opts *Options) { opts.RecoveryConcurrency = 2 })
	entered := make(chan string, 5)
	release := make(chan struct{})
	var active, peak atomic.Int64
	rt.RegisterHandler("replay", func(ctx context.Context, inv durable.Invocation) error {
		now := active.Add(1)
		defer active.Add(-1)
		for {
			seen := peak.Load()
			if now <= seen || peak.CompareAndSwap(seen, now) {
				break
			}
		}
		entered <- inv.Run().Key
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		_, err := inv.RunAction(ctx, "seen", func(context.Context) ([]byte, error) {
			return nil, errors.New("recorded action ran again")
		})
		return err
	})
	var fresh invocationLog
	rt.RegisterHandler("fresh", fresh.handler())
	startHost(t, rt)
	waitUntil(t, "two replays run and three wait", func() bool {
		s := rt.stats()
		return s.claimed == 5 && s.replaying == 2 && s.waiting == 3
	})
	receive(t, entered)
	receive(t, entered)
	mustStart(t, rt, durable.StartRequest{Handler: "fresh", Key: "k-fresh"})
	waitState(t, rt, runWant{key: "k-fresh", state: durable.RunSucceeded})
	if s := rt.stats(); s.replaying != 2 || s.waiting != 3 || len(entered) != 0 {
		t.Fatalf("after the fresh run: %+v, %d extra handlers entered", s, len(entered))
	}
	for range 5 {
		release <- struct{}{}
	}
	for _, key := range keys {
		waitState(t, rt, runWant{key: key, state: durable.RunSucceeded})
	}
	if peak.Load() != 2 {
		t.Fatalf("peak concurrent replays = %d, want 2", peak.Load())
	}
}

func TestLocalCancelIgnoresAnotherClaim(t *testing.T) {
	db := openDB(t, newDatabaseFile(t))
	insertRun(t, db, runFixture{key: "k-reclaimed", handler: "hold", claim: 3})
	rt := newRuntime(t, db, nil)
	entered := make(chan struct{}, 1)
	rt.RegisterHandler("hold", func(ctx context.Context, _ durable.Invocation) error {
		entered <- struct{}{}
		return parkUntilStopped(ctx)
	})
	startHost(t, rt)
	receive(t, entered)
	h := rt.currentHost()
	running := h.execution("k-reclaimed")
	if running == nil || running.claim != 4 {
		t.Fatalf("execution = %+v, want one under claim 4", running)
	}
	if exec := h.cancelLocal("k-reclaimed", h.currentOwner(), 3); exec != nil {
		t.Fatal("cancelLocal under an earlier claim returned the execution")
	}
	if running.cause() != nil {
		t.Fatalf("execution ended after a cancel under an earlier claim: %v", running.cause())
	}
	if row := readRun(t, db, "k-reclaimed"); row.state != string(durable.RunRunning) || row.claim != 4 {
		t.Fatalf("run = %+v, want running under claim 4", row)
	}
	if exec := h.cancelLocal("k-reclaimed", h.currentOwner(), 4); exec != running {
		t.Fatal("cancelLocal under the current claim did not return the execution")
	}
	if !errors.Is(running.cause(), durable.ErrRunCancelled) {
		t.Fatalf("cause = %v, want ErrRunCancelled", running.cause())
	}
	waitState(t, rt, runWant{key: "k-reclaimed", state: durable.RunCancelled})
}
