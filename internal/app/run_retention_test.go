package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/modules/broadcast"
	"github.com/anggasct/aura/internal/modules/child"
	"github.com/anggasct/aura/internal/modules/schedule"
	"github.com/anggasct/aura/internal/platform/durable/sqlite"
	"github.com/anggasct/aura/internal/platform/server"
)

const testRunRetention = 48 * time.Hour

type retentionRunSeed struct {
	key     string
	handler string
	state   string
	age     time.Duration
}

var (
	prunableRunSeeds = []retentionRunSeed{
		{key: "broadcast-old", handler: broadcast.HandlerName, state: "succeeded", age: 49 * time.Hour},
		{key: "broadcast-older", handler: broadcast.HandlerName, state: "failed", age: 50 * time.Hour},
		{key: "schedule-old", handler: schedule.HandlerName, state: "succeeded", age: 49 * time.Hour},
		{key: "schedule-older", handler: schedule.HandlerName, state: "cancelled", age: 51 * time.Hour},
		{key: "child-old", handler: child.HandlerName, state: "failed", age: 49 * time.Hour},
	}
	keptRunSeeds = []retentionRunSeed{
		{key: "workflow-old", handler: "workflow", state: "succeeded", age: 49 * time.Hour},
		{key: "turn-old", handler: "turn", state: "succeeded", age: 49 * time.Hour},
		{key: "schedule-recent", handler: schedule.HandlerName, state: "succeeded", age: 47 * time.Hour},
		{key: "schedule-running", handler: schedule.HandlerName, state: "running", age: 49 * time.Hour},
	}
)

func seedRetentionRuns(t *testing.T, db *sql.DB) {
	t.Helper()
	now := time.Now().UTC()
	for _, seed := range slices.Concat(prunableRunSeeds, keptRunSeeds) {
		stamp := now.Add(-seed.age).Format(time.RFC3339Nano)
		if _, err := db.ExecContext(t.Context(), `INSERT INTO durable_run (key, handler, payload, state, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
			seed.key, seed.handler, []byte("{}"), seed.state, stamp, stamp); err != nil {
			t.Fatalf("insert run %s: %v", seed.key, err)
		}
		if _, err := db.ExecContext(t.Context(), `INSERT INTO durable_action (run_key, action_key, outcome, result, created_at) VALUES (?, 'reply', 'result', ?, ?)`,
			seed.key, []byte(`"reply text"`), stamp); err != nil {
			t.Fatalf("insert journal of %s: %v", seed.key, err)
		}
	}
}

func seedKeys(seeds []retentionRunSeed) []string {
	keys := make([]string, 0, len(seeds))
	for _, seed := range seeds {
		keys = append(keys, seed.key)
	}
	slices.Sort(keys)
	return keys
}

func storedKeys(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), query)
	if err != nil {
		t.Fatalf("query keys: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatalf("scan key: %v", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read keys: %v", err)
	}
	slices.Sort(keys)
	return keys
}

func runKeys(t *testing.T, db *sql.DB) []string {
	t.Helper()
	return storedKeys(t, db, `SELECT key FROM durable_run`)
}

func journalKeys(t *testing.T, db *sql.DB) []string {
	t.Helper()
	return storedKeys(t, db, `SELECT DISTINCT run_key FROM durable_action`)
}

func requireOnlyKeptRuns(t *testing.T, db *sql.DB) {
	t.Helper()
	want := seedKeys(keptRunSeeds)
	if got := runKeys(t, db); !slices.Equal(got, want) {
		t.Errorf("runs after the sweep = %v, want %v", got, want)
	}
	if got := journalKeys(t, db); !slices.Equal(got, want) {
		t.Errorf("journals after the sweep = %v, want %v", got, want)
	}
}

type retentionRecord struct {
	Level   string `json:"level"`
	Msg     string `json:"msg"`
	Deleted int    `json:"deleted"`
}

func retentionRecords(t *testing.T, logs *syncBuffer) []retentionRecord {
	t.Helper()
	var records []retentionRecord
	for line := range strings.SplitSeq(logs.String(), "\n") {
		if line == "" {
			continue
		}
		var record retentionRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

func recordsAt(t *testing.T, logs *syncBuffer, level string) []retentionRecord {
	t.Helper()
	var matched []retentionRecord
	for _, record := range retentionRecords(t, logs) {
		if record.Level == level {
			matched = append(matched, record)
		}
	}
	return matched
}

func retentionRuntime(t *testing.T, db *sql.DB) *sqlite.Runtime {
	t.Helper()
	rt, err := sqlite.New(&sqlite.Options{DB: db, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	return rt
}

func newTestRunRetention(prune pruner, batch int, interval time.Duration) (*runRetention, *syncBuffer) {
	logs := &syncBuffer{}
	return &runRetention{
		pruner:    prune,
		retention: testRunRetention,
		batch:     batch,
		interval:  interval,
		logger:    slog.New(slog.NewJSONHandler(logs, nil)),
	}, logs
}

func startRunRetention(t *testing.T, sweep *runRetention) func() {
	t.Helper()
	task := server.Background("run_retention", sweep.run)
	if err := task.Start(t.Context()); err != nil {
		t.Fatalf("start the sweep: %v", err)
	}
	return func() {
		if err := task.Stop(t.Context()); err != nil {
			t.Errorf("stop the sweep: %v", err)
		}
	}
}

type failingOncePruner struct {
	next    pruner
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func (p *failingOncePruner) PruneEnded(ctx context.Context, req sqlite.PruneRequest) (int, error) {
	p.mu.Lock()
	p.calls++
	first := p.calls == 1
	p.mu.Unlock()
	if first {
		return 0, errors.New("database is locked")
	}
	select {
	case <-p.release:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	return p.next.PruneEnded(ctx, req)
}

func TestRunRetentionSweepPrunesEndedRuns(t *testing.T) {
	for _, batch := range []int{runRetentionBatch, 2} {
		t.Run("one sweep in batches of "+strconv.Itoa(batch), func(t *testing.T) {
			db := cronTestDB(t)
			seedRetentionRuns(t, db)
			sweep, logs := newTestRunRetention(retentionRuntime(t, db), batch, time.Hour)
			stop := startRunRetention(t, sweep)
			awaitSchedule(t, "the first sweep is logged", func() bool { return len(recordsAt(t, logs, "INFO")) > 0 })
			stop()
			requireOnlyKeptRuns(t, db)
			infos := recordsAt(t, logs, "INFO")
			if len(infos) != 1 || infos[0].Msg != "durable run retention" || infos[0].Deleted != len(prunableRunSeeds) {
				t.Errorf("sweep records = %+v, want one that deleted %d runs", infos, len(prunableRunSeeds))
			}
			if warnings := recordsAt(t, logs, "WARN"); len(warnings) != 0 {
				t.Errorf("warnings = %+v, want none", warnings)
			}
		})
	}

	t.Run("a failing prune ends that sweep and the next interval prunes again", func(t *testing.T) {
		db := cronTestDB(t)
		seedRetentionRuns(t, db)
		prune := &failingOncePruner{next: retentionRuntime(t, db), release: make(chan struct{})}
		sweep, logs := newTestRunRetention(prune, runRetentionBatch, 5*time.Millisecond)
		stop := startRunRetention(t, sweep)
		awaitSchedule(t, "the failed sweep is logged", func() bool { return len(recordsAt(t, logs, "WARN")) > 0 })
		if got, want := runKeys(t, db), seedKeys(slices.Concat(prunableRunSeeds, keptRunSeeds)); !slices.Equal(got, want) {
			t.Errorf("runs after the failed sweep = %v, want %v", got, want)
		}
		close(prune.release)
		awaitSchedule(t, "a later sweep deletes the ended runs", func() bool { return slices.Equal(runKeys(t, db), seedKeys(keptRunSeeds)) })
		stop()
		requireOnlyKeptRuns(t, db)
		warnings := recordsAt(t, logs, "WARN")
		if len(warnings) != 1 || warnings[0].Msg != "durable run retention failed" {
			t.Errorf("warnings = %+v, want one failed sweep", warnings)
		}
	})

	t.Run("a sweep whose context has ended logs nothing", func(t *testing.T) {
		db := cronTestDB(t)
		seedRetentionRuns(t, db)
		sweep, logs := newTestRunRetention(retentionRuntime(t, db), runRetentionBatch, time.Hour)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		sweep.run(ctx)
		if got := logs.String(); got != "" {
			t.Errorf("logs of a sweep under an ended context = %q, want none", got)
		}
	})
}
