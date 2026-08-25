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

// Shutdown ordering: the drain flag flips before the socket closes, and
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

	deadline := time.Now().Add(2 * time.Second)
	for !listener.readiness.Draining() {
		if time.Now().After(deadline) {
			t.Fatal("drain flag did not flip after cancellation")
		}
		time.Sleep(5 * time.Millisecond)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("listener did not stop")
	}

	if response, err := probeGet(t.Context(), addr, "/readyz"); err == nil {
		_ = response.Body.Close()
		t.Error("readyz still served after listener teardown")
	}
}
