package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/store/storetest"
)

const testTimeout = 20 * time.Second

func newDatabaseFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "aura.db")
	storetest.Seed(t, path)
	return path
}

func openDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := store.OpenDBWithOptions(t.Context(), path, store.OpenOptions{})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func newRuntime(t *testing.T, db *sql.DB, configure func(opts *Options)) *Runtime {
	t.Helper()
	opts := &Options{
		DB:           db,
		Logger:       slog.New(slog.DiscardHandler),
		PollInterval: 10 * time.Millisecond,
		StopGrace:    5 * time.Second,
	}
	if configure != nil {
		configure(opts)
	}
	rt, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return rt
}

type testHost struct {
	rt     *Runtime
	cancel context.CancelFunc
	result chan error
	once   sync.Once
	err    error
}

func startHost(t *testing.T, rt *Runtime) *testHost {
	t.Helper()
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	h := &testHost{rt: rt, cancel: cancel, result: make(chan error, 1)}
	go func() { h.result <- rt.Run(ctx) }()
	t.Cleanup(func() { _ = h.stop(t) })
	waitUntil(t, "host serves", func() bool { return rt.currentHost() != nil })
	return h
}

func (h *testHost) stop(t *testing.T) error {
	t.Helper()
	h.once.Do(func() {
		h.cancel()
		select {
		case h.err = <-h.result:
		case <-time.After(testTimeout):
			h.err = errors.New("host did not stop")
		}
	})
	return h.err
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.NewTimer(testTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for !cond() {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("timed out waiting until %s", what)
		}
	}
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for a value")
	}
	var zero T
	return zero
}

type runWant struct {
	key   string
	state durable.RunState
}

func waitState(t *testing.T, rt *Runtime, want runWant) durable.RunStatus {
	t.Helper()
	var status durable.RunStatus
	waitUntil(t, "run "+want.key+" is "+string(want.state), func() bool {
		got, err := rt.Status(t.Context(), durable.RunRef{Key: want.key})
		if err != nil {
			t.Fatalf("Status(%q): %v", want.key, err)
		}
		status = got
		return got.State == want.state
	})
	return status
}

func mustStart(t *testing.T, rt *Runtime, req durable.StartRequest) durable.RunRef {
	t.Helper()
	ref, err := rt.Start(t.Context(), req)
	if err != nil {
		t.Fatalf("Start(%q, %q): %v", req.Handler, req.Key, err)
	}
	return ref
}

type sentSignal struct {
	ref     durable.RunRef
	name    string
	payload string
}

func mustSignal(t *testing.T, rt *Runtime, sig sentSignal) {
	t.Helper()
	if err := rt.Signal(t.Context(), sig.ref, sig.name, []byte(sig.payload)); err != nil {
		t.Fatalf("Signal(%q, %q): %v", sig.ref.Key, sig.name, err)
	}
}

func requireCode(t *testing.T, err error, want ErrorCode) (detail string) {
	t.Helper()
	var coded *Error
	if !errors.As(err, &coded) || coded.Code != want {
		t.Fatalf("err = %v, want code %s", err, want)
	}
	return coded.Detail
}

type runFixture struct {
	key     string
	handler string
	owner   string
	claim   int64
}

func insertRun(t *testing.T, db *sql.DB, run runFixture) {
	t.Helper()
	var owner any
	if run.owner != "" {
		owner = run.owner
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.ExecContext(t.Context(), `INSERT INTO durable_run (key, handler, payload, state, owner, claim, created_at, updated_at)
VALUES (?, ?, ?, 'running', ?, ?, ?, ?)`, run.key, run.handler, []byte{}, owner, run.claim, stamp, stamp); err != nil {
		t.Fatalf("insert run %q: %v", run.key, err)
	}
}

type hostFixture struct {
	owner       string
	expiresInMS int64
}

func insertHostRow(t *testing.T, db *sql.DB, row hostFixture) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `INSERT INTO durable_host (owner, expires_at, created_at) VALUES (?, `+dbNowMS+` + ?, ?)`,
		row.owner, row.expiresInMS, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("insert host %q: %v", row.owner, err)
	}
}

func execSQL(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func countRows(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

type runRow struct {
	state           string
	detail          string
	owner           sql.NullString
	claim           int64
	cancelRequested bool
	updatedAt       string
}

func readRun(t *testing.T, db *sql.DB, key string) runRow {
	t.Helper()
	var row runRow
	if err := db.QueryRowContext(t.Context(), `SELECT state, detail, owner, claim, cancel_requested, updated_at FROM durable_run WHERE key = ?`, key).
		Scan(&row.state, &row.detail, &row.owner, &row.claim, &row.cancelRequested, &row.updatedAt); err != nil {
		t.Fatalf("read run %q: %v", key, err)
	}
	return row
}

type invocationLog struct {
	mu   sync.Mutex
	keys []string
}

func (l *invocationLog) add(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.keys = append(l.keys, key)
}

func (l *invocationLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.keys)
}

func (l *invocationLog) handler() durable.Handler {
	return func(_ context.Context, inv durable.Invocation) error {
		l.add(inv.Run().Key)
		return nil
	}
}

var childTables = []string{"durable_action", "durable_signal", "durable_wait", "durable_timer", "durable_approval"}

func insertChildren(t *testing.T, db *sql.DB, key string) {
	t.Helper()
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	execSQL(t, db, `INSERT INTO durable_action (run_key, action_key, outcome, result, detail, created_at) VALUES (?, 'step', 'result', ?, '', ?)`, key, []byte("x"), stamp)
	execSQL(t, db, `INSERT INTO durable_signal (run_key, name, seq, payload, created_at) VALUES (?, 'go', 0, ?, ?)`, key, []byte("x"), stamp)
	execSQL(t, db, `INSERT INTO durable_wait (run_key, name, seq, deadline, outcome, created_at, updated_at) VALUES (?, 'go', 0, ?, 'signal', ?, ?)`, key, stamp, stamp, stamp)
	execSQL(t, db, `INSERT INTO durable_timer (run_key, seq, fire_at, created_at) VALUES (?, 0, ?, ?)`, key, stamp, stamp)
	execSQL(t, db, `INSERT INTO durable_approval (approval_id, run_key, consumed_at) VALUES (?, ?, ?)`, durable.FormatApprovalAddr(key, "decision"), key, stamp)
}

func childRows(t *testing.T, db *sql.DB, key string) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for _, table := range childTables {
		if n := countRows(t, db, `SELECT COUNT(*) FROM `+table+` WHERE run_key = ?`, key); n > 0 {
			counts[table] = n
		}
	}
	return counts
}
