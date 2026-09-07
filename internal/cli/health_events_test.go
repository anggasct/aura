package cli

import (
	"context"
	"net"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/health"
	"github.com/anggasct/aura/internal/store"
)

func baseTime() time.Time { return time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC) }

func TestHealthEventLogRoundTrip(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenDB(ctx, filepath.Join(t.TempDir(), "aura.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	log := newHealthEventLog(store.NewEventStore(db), store.NewSessionService(db))

	first := health.Transition{FindingID: "backup/ok", From: health.StatusNone, To: health.StatusUp, Code: "ok", At: baseTime()}
	second := health.Transition{FindingID: "backup/ok", From: health.StatusUp, To: health.StatusDegraded, Code: "backup_stale", At: baseTime().Add(time.Hour)}
	if err := log.sink(ctx, &first); err != nil {
		t.Fatalf("sink first: %v", err)
	}
	if err := log.sink(ctx, &second); err != nil {
		t.Fatalf("sink second: %v", err)
	}

	history, err := log.history(ctx)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if !slices.Equal(history, []health.Transition{first, second}) {
		t.Fatalf("history = %+v", history)
	}

	tracker, err := health.NewStateTracker(health.TransitionPolicy{StableFor: time.Minute, Cooldown: time.Minute}, log.sink, log.history)
	if err != nil {
		t.Fatalf("rebuild tracker: %v", err)
	}
	if _, err := tracker.Observe(ctx, []health.Finding{{ID: "backup/ok", Status: health.StatusDegraded}}); err != nil {
		t.Fatal(err)
	}
	if snapshot := tracker.Snapshot(); len(snapshot) != 1 || snapshot[0].Status != health.StatusDegraded {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestShutdownFlipsDrainingBeforeSocketCloses(t *testing.T) {
	dataRoot := t.TempDir()
	db, err := store.OpenDB(t.Context(), filepath.Join(dataRoot, "aura.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(t.Context(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cfg := config.Default()
	cfg.Storage.Path = dataRoot
	listener, err := buildProbeListener(&cfg, nil, store.NewEventStore(db), store.NewSessionService(db))
	if err != nil {
		t.Fatalf("buildProbeListener: %v", err)
	}

	bind, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	addr := bind.Addr().String()
	_ = bind.Close()
	listener.listen = addr

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- listener.Start(ctx) }()
	waitForProbe(t, addr, "/livez")

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("listener did not stop")
	}
	if !listener.readiness.Draining() {
		t.Fatal("drain flag not set when Start returned; ordering violated")
	}
	if response, err := probeGet(t.Context(), addr, "/readyz"); err == nil {
		_ = response.Body.Close()
		t.Error("readyz still served after listener teardown")
	}
}

func TestObserverPersistsTransitionsReplayableAfterRestart(t *testing.T) {
	ctx := context.Background()
	dataRoot := t.TempDir()
	db, err := store.OpenDB(ctx, filepath.Join(dataRoot, "aura.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	expired := make(chan struct{})
	var expiredOnce sync.Once
	evaluate := func(evalCtx context.Context) []health.Finding {
		<-evalCtx.Done()
		expiredOnce.Do(func() { close(expired) })
		return []health.Finding{{ID: "backup/backup_missing", Status: health.StatusDown, Code: "backup_missing"}}
	}
	log := newHealthEventLog(store.NewEventStore(db), store.NewSessionService(db))
	var persistOnce sync.Once
	persistDone := make(chan struct{})
	sink := func(persistCtx context.Context, transition *health.Transition) error {
		err := log.sink(persistCtx, transition)
		persistOnce.Do(func() { close(persistDone) })
		return err
	}
	tracker, err := health.NewStateTracker(
		health.TransitionPolicy{StableFor: time.Nanosecond, Cooldown: time.Nanosecond},
		sink,
		nil,
	)
	if err != nil {
		t.Fatalf("tracker: %v", err)
	}
	listener := &probeListener{evaluate: evaluate, tracker: tracker, interval: 10 * time.Millisecond}

	observerCtx, stopObserver := context.WithCancel(ctx)
	observerDone := listener.startObserver(observerCtx)

	select {
	case <-expired:
	case <-time.After(5 * time.Second):
		stopObserver()
		<-observerDone
		t.Fatal("evaluation context never expired")
	}
	select {
	case <-persistDone:
	case <-time.After(5 * time.Second):
		stopObserver()
		<-observerDone
		t.Fatal("durable append never completed after evaluation expired")
	}
	stopObserver()
	<-observerDone

	check := newHealthEventLog(store.NewEventStore(db), store.NewSessionService(db))
	history, err := check.history(ctx)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 1 || history[0].FindingID != "backup/backup_missing" || history[0].From != health.StatusNone {
		t.Fatalf("persisted history = %+v", history)
	}

	restarted := newHealthEventLog(store.NewEventStore(db), store.NewSessionService(db))
	replayed, err := restarted.history(ctx)
	if err != nil {
		t.Fatalf("history after restart: %v", err)
	}
	if !slices.Equal(replayed, history) {
		t.Fatalf("replayed history drifted: %+v vs %+v", replayed, history)
	}
	fresh, err := health.NewStateTracker(health.TransitionPolicy{StableFor: time.Minute, Cooldown: time.Minute}, restarted.sink, restarted.history)
	if err != nil {
		t.Fatalf("rebuild tracker: %v", err)
	}
	if snapshot := fresh.Snapshot(); len(snapshot) == 0 {
		t.Fatal("restored tracker has no state")
	}
}

func TestObserverStopCancelsInFlightPersistence(t *testing.T) {
	sinkStarted := make(chan struct{})
	var sawCancellation atomic.Bool
	slowSink := func(sinkCtx context.Context, _ *health.Transition) error {
		select {
		case sinkStarted <- struct{}{}:
		default:
		}
		<-sinkCtx.Done()
		sawCancellation.Store(true)
		return sinkCtx.Err()
	}
	tracker, err := health.NewStateTracker(health.TransitionPolicy{StableFor: time.Nanosecond, Cooldown: time.Nanosecond}, slowSink, nil)
	if err != nil {
		t.Fatalf("tracker: %v", err)
	}
	listener := &probeListener{
		evaluate: func(context.Context) []health.Finding {
			return []health.Finding{{ID: "f/one", Status: health.StatusDown, Code: "down"}}
		},
		tracker:  tracker,
		interval: 10 * time.Millisecond,
	}

	observerCtx, stop := context.WithCancel(context.Background())
	observerDone := listener.startObserver(observerCtx)

	<-sinkStarted
	stoppedAt := time.Now()
	stop()
	<-observerDone
	if elapsed := time.Since(stoppedAt); elapsed > 5*time.Second {
		t.Errorf("stop blocked %v; in-flight persistence was not cancelled", elapsed)
	}
	if !sawCancellation.Load() {
		t.Error("in-flight sink observed no cancellation")
	}
}

func TestHistoryPagesBeyondFirstThousandEvents(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenDB(ctx, filepath.Join(dataRoot(t), "aura.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	log := newHealthEventLog(store.NewEventStore(db), store.NewSessionService(db))
	stamp := baseTime()
	for i := range 1200 {
		status := health.StatusUp
		if i == 1199 {
			status = health.StatusDown
		}
		transition := health.Transition{FindingID: "backup/ok", From: health.StatusUp, To: status, Code: "ok", At: stamp}
		if err := log.sink(ctx, &transition); err != nil {
			t.Fatalf("sink %d: %v", i, err)
		}
	}
	history, err := log.history(ctx)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 1200 {
		t.Fatalf("history length = %d, want 1200 (no oldest-event cap)", len(history))
	}
	if history[len(history)-1].To != health.StatusDown {
		t.Fatalf("last replayed transition = %+v, want the sequence-final down state", history[len(history)-1])
	}
}

func dataRoot(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}
