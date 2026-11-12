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
	"github.com/anggasct/aura/internal/platform/turn/engine"
	"github.com/anggasct/aura/internal/store"
)

const (
	testRunRetention = 48 * time.Hour
	heldTurnKey      = "turn-held"
	heldTurnSession  = "lobby/discord:owner/dm:601"
	liveTurnSession  = "lobby/discord:owner/dm:602"
	goneTurnSession  = "lobby/discord:owner/dm:600"
)

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
		{key: "turn-old", handler: runtimeengine.HandlerName, state: "succeeded", age: 49 * time.Hour},
		{key: "turn-older", handler: runtimeengine.HandlerName, state: "cancelled", age: 52 * time.Hour},
	}
	keptRunSeeds = []retentionRunSeed{
		{key: "workflow-old", handler: "workflow", state: "succeeded", age: 49 * time.Hour},
		{key: "turn-running", handler: runtimeengine.HandlerName, state: "running", age: 49 * time.Hour},
		{key: heldTurnKey, handler: runtimeengine.HandlerName, state: "succeeded", age: 49 * time.Hour},
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
	seedAcceptedTurn(t, db, acceptedTurnSeed{session: heldTurnSession, turn: heldTurnKey})
	if _, err := db.ExecContext(t.Context(), `INSERT INTO durable_session_turn (turn_id, session_id, sequence, state, descriptor_json, created_at, updated_at) VALUES (?, ?, 1, 'active', '{}', ?, ?)`,
		heldTurnKey, heldTurnSession, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("hold the turn %s in its session: %v", heldTurnKey, err)
	}
}

type acceptedTurnSeed struct {
	session string
	turn    string
}

func seedAcceptedTurn(t *testing.T, db *sql.DB, seed acceptedTurnSeed) {
	t.Helper()
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.ExecContext(t.Context(), `INSERT INTO session (id, owner_id, agent_id, metadata_json, created_at, updated_at) VALUES (?, 'discord:601', 'lobby', '{}', ?, ?)`,
		seed.session, stamp, stamp); err != nil {
		t.Fatalf("insert session %s: %v", seed.session, err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO runtime_event (id, session_id, sequence, turn_id, invocation_id, author, kind, schema_version, payload_json, created_at) VALUES (?, ?, 1, ?, '', 'discord:601', 'turn.accepted', 1, '{}', ?)`,
		"accepted-"+seed.turn, seed.session, seed.turn, stamp); err != nil {
		t.Fatalf("insert the accepted event of %s: %v", seed.turn, err)
	}
}

type orphanRunSeed struct {
	key     string
	state   string
	payload string
	orphan  bool
}

var orphanRunSeeds = []orphanRunSeed{
	{key: "turn-orphan-running", state: "running", payload: `{"turn_id":"turn-orphan-running","session_id":"` + goneTurnSession + `"}`, orphan: true},
	{key: "turn-orphan-ended", state: "failed", payload: `{"turn_id":"turn-orphan-ended","session_id":"` + goneTurnSession + `"}`, orphan: true},
	{key: "turn-accepted", state: "succeeded", payload: `{"turn_id":"turn-accepted","session_id":"` + liveTurnSession + `"}`},
	{key: "turn-not-json", state: "failed", payload: `not json`},
}

func seedOrphanRuns(t *testing.T, db *sql.DB) []string {
	t.Helper()
	stamp := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	var kept []string
	for _, seed := range orphanRunSeeds {
		if _, err := db.ExecContext(t.Context(), `INSERT INTO durable_run (key, handler, payload, state, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
			seed.key, runtimeengine.HandlerName, []byte(seed.payload), seed.state, stamp, stamp); err != nil {
			t.Fatalf("insert run %s: %v", seed.key, err)
		}
		if _, err := db.ExecContext(t.Context(), `INSERT INTO durable_action (run_key, action_key, outcome, result, created_at) VALUES (?, 'reply', 'result', ?, ?)`,
			seed.key, []byte(`"reply text"`), stamp); err != nil {
			t.Fatalf("insert journal of %s: %v", seed.key, err)
		}
		if !seed.orphan {
			kept = append(kept, seed.key)
		}
	}
	seedAcceptedTurn(t, db, acceptedTurnSeed{session: liveTurnSession, turn: "turn-accepted"})
	slices.Sort(kept)
	return kept
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
	Level    string `json:"level"`
	Msg      string `json:"msg"`
	Deleted  int    `json:"deleted"`
	Orphaned int    `json:"orphaned"`
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

func newTestRunRetention(t *testing.T, db *sql.DB, interval time.Duration) (*runRetention, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	return &runRetention{
		pruner:    retentionRuntime(t, db),
		orphans:   func(ctx context.Context) (int, error) { return store.DeleteOrphanedTurnRuns(ctx, db) },
		retention: testRunRetention,
		batch:     runRetentionBatch,
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
			sweep, logs := newTestRunRetention(t, db, time.Hour)
			sweep.batch = batch
			stop := startRunRetention(t, sweep)
			awaitSchedule(t, "the first sweep is logged", func() bool { return len(recordsAt(t, logs, "INFO")) > 0 })
			stop()
			requireOnlyKeptRuns(t, db)
			infos := recordsAt(t, logs, "INFO")
			if len(infos) != 1 || infos[0].Msg != "durable run retention" || infos[0].Deleted != len(prunableRunSeeds) || infos[0].Orphaned != 0 {
				t.Errorf("sweep records = %+v, want one that deleted %d runs and no orphan", infos, len(prunableRunSeeds))
			}
			if warnings := recordsAt(t, logs, "WARN"); len(warnings) != 0 {
				t.Errorf("warnings = %+v, want none", warnings)
			}
		})
	}

	t.Run("a failing prune ends that sweep and the next interval prunes again", func(t *testing.T) {
		db := cronTestDB(t)
		seedRetentionRuns(t, db)
		sweep, logs := newTestRunRetention(t, db, 5*time.Millisecond)
		prune := &failingOncePruner{next: sweep.pruner, release: make(chan struct{})}
		sweep.pruner = prune
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
		if len(warnings) != 1 || warnings[0].Msg != "durable run retention failed" || warnings[0].Deleted != 0 || warnings[0].Orphaned != 0 {
			t.Errorf("warnings = %+v, want one failed sweep", warnings)
		}
	})

	t.Run("a failing prune reports the orphans its sweep deleted", func(t *testing.T) {
		db := cronTestDB(t)
		kept := seedOrphanRuns(t, db)
		sweep, logs := newTestRunRetention(t, db, time.Hour)
		sweep.pruner = &failingOncePruner{next: sweep.pruner, release: make(chan struct{})}
		stop := startRunRetention(t, sweep)
		awaitSchedule(t, "the failed sweep is logged", func() bool { return len(recordsAt(t, logs, "WARN")) > 0 })
		stop()
		if got := runKeys(t, db); !slices.Equal(got, kept) {
			t.Errorf("runs after the failed sweep = %v, want %v", got, kept)
		}
		warnings := recordsAt(t, logs, "WARN")
		if len(warnings) != 1 || warnings[0].Msg != "durable run retention failed" || warnings[0].Orphaned != 2 || warnings[0].Deleted != 0 {
			t.Errorf("warnings = %+v, want one failed sweep that deleted 2 orphans", warnings)
		}
		if infos := recordsAt(t, logs, "INFO"); len(infos) != 0 {
			t.Errorf("info records of the failed sweep = %+v, want none", infos)
		}
	})

	t.Run("a sweep whose context has ended logs nothing", func(t *testing.T) {
		db := cronTestDB(t)
		seedRetentionRuns(t, db)
		sweep, logs := newTestRunRetention(t, db, time.Hour)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		sweep.run(ctx)
		if got := logs.String(); got != "" {
			t.Errorf("logs of a sweep under an ended context = %q, want none", got)
		}
	})
}

type failingOnceOrphans struct {
	next    func(ctx context.Context) (int, error)
	retried chan struct{}
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func (o *failingOnceOrphans) delete(ctx context.Context) (int, error) {
	o.mu.Lock()
	o.calls++
	calls := o.calls
	o.mu.Unlock()
	switch calls {
	case 1:
		return 0, errors.New("database is locked")
	case 2:
		close(o.retried)
		select {
		case <-o.release:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return o.next(ctx)
}

func TestRunRetentionSweepDeletesOrphanedTurnRuns(t *testing.T) {
	t.Run("one sweep deletes the orphans of every state", func(t *testing.T) {
		db := cronTestDB(t)
		kept := seedOrphanRuns(t, db)
		sweep, logs := newTestRunRetention(t, db, time.Hour)
		stop := startRunRetention(t, sweep)
		awaitSchedule(t, "the first sweep is logged", func() bool { return len(recordsAt(t, logs, "INFO")) > 0 })
		stop()
		if got := runKeys(t, db); !slices.Equal(got, kept) {
			t.Errorf("runs after the sweep = %v, want %v", got, kept)
		}
		if got := journalKeys(t, db); !slices.Equal(got, kept) {
			t.Errorf("journals after the sweep = %v, want %v", got, kept)
		}
		infos := recordsAt(t, logs, "INFO")
		if len(infos) != 1 || infos[0].Msg != "durable run retention" || infos[0].Orphaned != 2 || infos[0].Deleted != 0 {
			t.Errorf("sweep records = %+v, want one that deleted 2 orphans", infos)
		}
		if warnings := recordsAt(t, logs, "WARN"); len(warnings) != 0 {
			t.Errorf("warnings = %+v, want none", warnings)
		}
	})

	t.Run("a failing orphan step ends that sweep and the next interval runs both steps", func(t *testing.T) {
		db := cronTestDB(t)
		seedRetentionRuns(t, db)
		kept := slices.Sorted(slices.Values(slices.Concat(seedKeys(keptRunSeeds), seedOrphanRuns(t, db))))
		seeded := len(prunableRunSeeds) + len(keptRunSeeds) + len(orphanRunSeeds)
		sweep, logs := newTestRunRetention(t, db, 5*time.Millisecond)
		orphans := &failingOnceOrphans{next: sweep.orphans, retried: make(chan struct{}), release: make(chan struct{})}
		sweep.orphans = orphans.delete
		stop := startRunRetention(t, sweep)
		select {
		case <-orphans.retried:
		case <-time.After(scheduleWaitDeadline):
			t.Fatal("timed out waiting for the sweep after the failed one")
		}
		if infos := recordsAt(t, logs, "INFO"); len(infos) != 0 {
			t.Errorf("info records of the failed sweep = %+v, want none", infos)
		}
		if warnings := recordsAt(t, logs, "WARN"); len(warnings) != 1 || warnings[0].Msg != "durable run retention failed" || warnings[0].Orphaned != 0 || warnings[0].Deleted != 0 {
			t.Errorf("warnings = %+v, want one failed sweep that deleted nothing", warnings)
		}
		if got := len(runKeys(t, db)); got != seeded {
			t.Errorf("runs after the failed sweep = %d, want every one of the %d seeded", got, seeded)
		}
		close(orphans.release)
		awaitSchedule(t, "a later sweep is logged", func() bool { return len(recordsAt(t, logs, "INFO")) > 0 })
		stop()
		if got := runKeys(t, db); !slices.Equal(got, kept) {
			t.Errorf("runs after the later sweep = %v, want %v", got, kept)
		}
		if infos := recordsAt(t, logs, "INFO"); infos[0].Orphaned != 2 || infos[0].Deleted != len(prunableRunSeeds) {
			t.Errorf("first info record = %+v, want 2 orphans and %d pruned runs", infos[0], len(prunableRunSeeds))
		}
		if warnings := recordsAt(t, logs, "WARN"); len(warnings) != 1 {
			t.Errorf("warnings = %+v, want one failed sweep", warnings)
		}
	})
}
