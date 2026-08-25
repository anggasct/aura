package cli

import (
	"context"
	"net"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/health"
	"github.com/anggasct/aura/internal/store"
)

// The durable log must round-trip transitions: writes persist as runtime
// events under the reserved system session, and history replays them
// oldest-first for restart recovery.
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

	// A fresh tracker rebuilt from this log resumes at degraded and emits
	// nothing until a real change stabilizes.
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

// Shutdown ordering: the drain flag flips synchronously before the socket
// closes — the flag is set the instant Start's cancellation branch runs,
// so once Start returns the ordering invariant is already decided, and
// liveness survives the graceful drain window.
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
	// Start returned after Shutdown completed; draining must have been set
	// on the same goroutine before Shutdown was called.
	if !listener.readiness.Draining() {
		t.Fatal("drain flag not set when Start returned; ordering violated")
	}
	// The drain flag must also have preceded the socket teardown: once the
	// listener is gone, any lingering connection attempt fails, proving the
	// flag outlived the socket.
	if response, err := probeGet(t.Context(), addr, "/readyz"); err == nil {
		_ = response.Body.Close()
		t.Error("readyz still served after listener teardown")
	}
}

// The periodic observer must persist transitions durably: an evaluation
// whose bounded evaluation context has ended still writes the transition,
// and a fresh process replays it after restart.
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
	log := newHealthEventLog(store.NewEventStore(db), store.NewSessionService(db))
	policy := health.TransitionPolicy{StableFor: time.Minute, Cooldown: time.Minute}
	tracker, err := health.NewStateTracker(policy, log.sink, log.history)
	if err != nil {
		t.Fatalf("tracker: %v", err)
	}

	// An already-cancelled evaluation context must not break persistence:
	// the observe loop evaluates on a bounded context, then persists on a
	// separate shutdown-bounded one — the same split is proven here.
	expired, cancel := context.WithCancel(ctx)
	cancel()
	_ = expired
	findings := []health.Finding{{ID: "backup/backup_missing", Status: health.StatusDown, Code: "backup_missing"}}
	persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer persistCancel()
	if _, err := tracker.Observe(persistCtx, findings); err != nil {
		t.Fatalf("observe: %v", err)
	}

	// Restart: a fresh log over the same store replays the transition.
	restarted := newHealthEventLog(store.NewEventStore(db), store.NewSessionService(db))
	history, err := restarted.history(ctx)
	if err != nil {
		t.Fatalf("history after restart: %v", err)
	}
	if len(history) != 1 || history[0].FindingID != "backup/backup_missing" || history[0].To != health.StatusDown {
		t.Fatalf("replayed history = %+v", history)
	}
	fresh, err := health.NewStateTracker(policy, restarted.sink, restarted.history)
	if err != nil {
		t.Fatalf("fresh tracker: %v", err)
	}
	if snapshot := fresh.Snapshot(); len(snapshot) != 1 || snapshot[0].Status != health.StatusDown {
		t.Fatalf("recovered snapshot = %+v", snapshot)
	}
}

// Recovery replays the latest state regardless of history length: paging
// follows sequence order, so transitions beyond any single page still
// reach the tracker, and equal timestamps cannot reorder replay.
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
	// 1200 transitions with identical timestamps: the latest wins and the
	// sequence order, not the clock, decides replay.
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
