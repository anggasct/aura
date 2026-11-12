package app

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/modules/broadcast"
	"github.com/anggasct/aura/internal/modules/schedule"
	"github.com/anggasct/aura/internal/platform/durable"
)

const (
	compositionServerWait = 30 * time.Second
	probeStopDelay        = 300 * time.Millisecond
	hostOutlivesRunWait   = time.Second
)

type compositionServer struct {
	app    *App
	cancel context.CancelFunc
	ran    chan error
}

func buildCompositionServer(t *testing.T, configPath string) *compositionServer {
	t.Helper()
	srv := &compositionServer{app: buildApp(t, serverCompositionOptions(loadCompositionConfig(t, configPath), &syncBuffer{}))}
	t.Cleanup(func() {
		if srv.ran != nil {
			srv.cancel()
			<-srv.ran
		}
		_ = srv.app.Close(context.Background())
	})
	return srv
}

func (s *compositionServer) run(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	s.cancel, s.ran = cancel, make(chan error, 1)
	go func() { s.ran <- s.app.Run(ctx) }()
}

func (s *compositionServer) stopRun(t *testing.T) {
	t.Helper()
	s.cancel()
	select {
	case err := <-s.ran:
		s.ran = nil
		if err != nil {
			t.Fatalf("Run after cancel = %v, want nil", err)
		}
	case <-time.After(compositionServerWait):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

func (s *compositionServer) close(t *testing.T) {
	t.Helper()
	s.stopRun(t)
	if err := s.app.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func compositionDatabase(t *testing.T, configPath string) *sql.DB {
	t.Helper()
	dbPath, _, _, err := storagePaths(loadCompositionConfig(t, configPath).Config)
	if err != nil {
		t.Fatalf("storagePaths: %v", err)
	}
	return openDatabaseFile(t, dbPath)
}

type sleepingProbe struct {
	invoked    chan struct{}
	returned   chan struct{}
	once       sync.Once
	returnOnce sync.Once
}

func (p *sleepingProbe) handle(_ context.Context, inv durable.Invocation) error {
	p.once.Do(func() { close(p.invoked) })
	err := inv.Sleep(time.Hour)
	<-time.After(probeStopDelay)
	p.returnOnce.Do(func() { close(p.returned) })
	return err
}

func (p *sleepingProbe) hasReturned() bool {
	select {
	case <-p.returned:
		return true
	default:
		return false
	}
}

func TestCompositionServerHostsTheDurableRuntime(t *testing.T) {
	restoreDefaultLogger(t)
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	path := writeCompositionConfig(t, freeLoopbackAddress(t), compositionModelsYAML, "")
	srv := buildCompositionServer(t, path)
	probe := &sleepingProbe{invoked: make(chan struct{}), returned: make(chan struct{})}
	registrar, ok := srv.app.runtime.(durable.HandlerRegistrar)
	if !ok {
		t.Fatalf("the server runtime %T takes no handlers", srv.app.runtime)
	}
	registrar.RegisterHandler("probe", probe.handle)
	srv.run(t)
	if _, err := srv.app.runtime.Start(t.Context(), durable.StartRequest{Handler: "probe", Key: "probe-sleeping"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-probe.invoked:
	case <-time.After(compositionServerWait):
		t.Fatal("the host did not invoke the probe run")
	}
	if n := countRows(t, srv.app.db, `SELECT COUNT(*) FROM durable_host`); n != 1 {
		t.Fatalf("lease rows while the server runs = %d, want 1", n)
	}
	srv.stopRun(t)
	select {
	case <-probe.returned:
		t.Fatal("the probe returned when the command context ended")
	case <-time.After(hostOutlivesRunWait):
	}
	if n := countRows(t, srv.app.db, `SELECT COUNT(*) FROM durable_host`); n != 1 {
		t.Fatalf("lease rows after Run returned = %d, want 1", n)
	}
	if err := srv.app.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	db := compositionDatabase(t, path)
	if n := countRows(t, db, `SELECT COUNT(*) FROM durable_host`); n != 0 {
		t.Fatalf("lease rows after Close = %d, want 0", n)
	}
	if !probe.hasReturned() {
		t.Fatal("Close returned before the probe did")
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM durable_run WHERE key = 'probe-sleeping' AND state = 'running' AND owner IS NULL`); n != 1 {
		t.Fatalf("released probe runs = %d, want 1", n)
	}
}

func TestServerHostClaimsARunACommandStarted(t *testing.T) {
	restoreDefaultLogger(t)
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	path := writeCompositionConfig(t, freeLoopbackAddress(t), compositionModelsYAML, "")
	srv := buildCompositionServer(t, path)
	srv.run(t)
	command := buildApp(t, commandCompositionOptions(loadCompositionConfig(t, path), &syncBuffer{}, PartDurable))
	t.Cleanup(func() { _ = command.Close(context.Background()) })
	if err := broadcast.StartItemRun(t.Context(), command.runtime, "bcst-missing"); err != nil {
		t.Fatalf("StartItemRun: %v", err)
	}
	awaitSchedule(t, "the server host ran the command's run", func() bool {
		status, err := command.runtime.Status(t.Context(), durable.RunRef{Key: "bcst-missing"})
		return err == nil && status.State == durable.RunSucceeded
	})
	srv.close(t)
}

type timerRow struct {
	seq       int64
	fireAt    string
	createdAt string
}

func readTimerRow(t *testing.T, db *sql.DB, key string) timerRow {
	t.Helper()
	var row timerRow
	if err := db.QueryRowContext(t.Context(), `SELECT seq, fire_at, created_at FROM durable_timer WHERE run_key = ?`, key).Scan(&row.seq, &row.fireAt, &row.createdAt); err != nil {
		t.Fatalf("read the timer row: %v", err)
	}
	return row
}

func TestServerContinuesASleepingRunAfterARestart(t *testing.T) {
	restoreDefaultLogger(t)
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	path := writeCompositionConfig(t, freeLoopbackAddress(t), compositionModelsYAML, "")
	first := buildCompositionServer(t, path)
	db := compositionDatabase(t, path)
	first.run(t)
	instant := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Minute)
	if err := schedule.StartFire(t.Context(), &schedule.FireRequest{Starter: first.app.runtime, JobID: "job-ahead", Instant: instant}); err != nil {
		t.Fatalf("StartFire: %v", err)
	}
	key := scheduleRunKey("job-ahead", instant)
	awaitSchedule(t, "the fire sleeps on an owned run", func() bool {
		return countRows(t, db, `SELECT COUNT(*) FROM durable_timer WHERE run_key = ?`, key) == 1 &&
			countRows(t, db, `SELECT COUNT(*) FROM durable_run WHERE key = ? AND owner IS NOT NULL`, key) == 1
	})
	timer := readTimerRow(t, db, key)
	runs := countRows(t, db, `SELECT COUNT(*) FROM durable_run`)
	first.close(t)
	if n := countRows(t, db, `SELECT COUNT(*) FROM durable_run WHERE key = ? AND state = 'running' AND owner IS NULL`, key); n != 1 {
		t.Fatalf("released runs after the stop = %d, want 1", n)
	}
	second := buildCompositionServer(t, path)
	second.run(t)
	awaitSchedule(t, "the second server claims the run", func() bool {
		return countRows(t, db, `SELECT COUNT(*) FROM durable_run WHERE key = ? AND owner IS NOT NULL AND claim = 2`, key) == 1
	})
	if n := countRows(t, db, `SELECT COUNT(*) FROM durable_run`); n != runs {
		t.Fatalf("durable_run rows after the restart = %d, want %d", n, runs)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM durable_timer WHERE run_key = ?`, key); n != 1 {
		t.Fatalf("timer rows after the restart = %d, want 1", n)
	}
	if got := readTimerRow(t, db, key); got != timer {
		t.Fatalf("timer row after the restart = %+v, want %+v", got, timer)
	}
	second.close(t)
}
