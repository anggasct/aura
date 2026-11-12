package sqlite

import (
	"database/sql"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
)

var pruneBase = time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)

type stampedRun struct {
	key     string
	handler string
	state   durable.RunState
	at      time.Time
}

func insertStampedRun(t *testing.T, db *sql.DB, run stampedRun) {
	t.Helper()
	insertRun(t, db, runFixture{key: run.key, handler: run.handler})
	execSQL(t, db, `UPDATE durable_run SET state = ?, updated_at = ? WHERE key = ?`, string(run.state), run.at.Format(time.RFC3339Nano), run.key)
	insertChildren(t, db, run.key)
}

func runKeys(t *testing.T, db *sql.DB) []string {
	t.Helper()
	return queryStrings(t, db, `SELECT key FROM durable_run ORDER BY key`)
}

func mustPrune(t *testing.T, rt *Runtime, req PruneRequest) int {
	t.Helper()
	n, err := rt.PruneEnded(t.Context(), req)
	if err != nil {
		t.Fatalf("PruneEnded: %v", err)
	}
	return n
}

func TestPruneEndedDeletesOldEndedRuns(t *testing.T) {
	db := openDB(t, newDatabaseFile(t))
	rt := newRuntime(t, db, nil)
	pruned := []stampedRun{
		{key: "k-succeeded", handler: "job", state: durable.RunSucceeded, at: pruneBase.Add(time.Second)},
		{key: "k-failed", handler: "job", state: durable.RunFailed, at: pruneBase.Add(2 * time.Second)},
		{key: "k-cancelled", handler: "job", state: durable.RunCancelled, at: pruneBase.Add(4900 * time.Millisecond)},
	}
	kept := []stampedRun{
		{key: "k-after", handler: "job", state: durable.RunSucceeded, at: pruneBase.Add(6 * time.Second)},
		{key: "k-same-second", handler: "job", state: durable.RunFailed, at: pruneBase.Add(5100 * time.Millisecond)},
		{key: "k-running", handler: "job", state: durable.RunRunning, at: pruneBase},
		{key: "k-unnamed", handler: "other", state: durable.RunSucceeded, at: pruneBase.Add(time.Second)},
		{key: "k-turn", handler: "job", state: durable.RunSucceeded, at: pruneBase.Add(time.Second)},
	}
	for _, run := range slices.Concat(pruned, kept) {
		insertStampedRun(t, db, run)
	}
	createSession(t, db, "session-prune")
	mustInsertHeldTurn(t, db, heldRow{sessionID: "session-prune", turnID: "k-turn", sequence: 1, state: turnStateActive})
	req := PruneRequest{Handlers: []string{"job", "unused"}, Before: pruneBase.Add(5 * time.Second), Limit: 100}
	if n := mustPrune(t, rt, req); n != len(pruned) {
		t.Fatalf("pruned %d runs, want %d", n, len(pruned))
	}
	for _, run := range pruned {
		if n := countRows(t, db, `SELECT COUNT(*) FROM durable_run WHERE key = ?`, run.key); n != 0 {
			t.Fatalf("run %q was not pruned", run.key)
		}
		if got := childRows(t, db, run.key); len(got) != 0 {
			t.Fatalf("journal of pruned run %q = %v, want none", run.key, got)
		}
	}
	for _, run := range kept {
		if row := readRun(t, db, run.key); row.state != string(run.state) {
			t.Fatalf("kept run %q = %+v", run.key, row)
		}
		if got := childRows(t, db, run.key); len(got) != len(childTables) {
			t.Fatalf("journal of kept run %q = %v, want it whole", run.key, got)
		}
	}
	if n := mustPrune(t, rt, req); n != 0 {
		t.Fatalf("second prune deleted %d runs, want 0", n)
	}
}

func TestPruneEndedHonoursItsLimit(t *testing.T) {
	db := openDB(t, newDatabaseFile(t))
	rt := newRuntime(t, db, nil)
	for _, second := range []int{3, 0, 4, 1, 2} {
		insertStampedRun(t, db, stampedRun{
			key:     fmt.Sprintf("k-at-%d", second),
			handler: "job",
			state:   durable.RunSucceeded,
			at:      pruneBase.Add(time.Duration(second) * time.Second),
		})
	}
	req := PruneRequest{Handlers: []string{"job"}, Before: pruneBase.Add(time.Minute), Limit: 2}
	steps := []struct {
		deleted int
		left    []string
	}{
		{deleted: 2, left: []string{"k-at-2", "k-at-3", "k-at-4"}},
		{deleted: 2, left: []string{"k-at-4"}},
		{deleted: 1, left: nil},
	}
	for i, step := range steps {
		if n := mustPrune(t, rt, req); n != step.deleted {
			t.Fatalf("prune %d deleted %d runs, want %d", i+1, n, step.deleted)
		}
		if got := runKeys(t, db); !slices.Equal(got, step.left) {
			t.Fatalf("runs after prune %d = %q, want %q", i+1, got, step.left)
		}
	}
}

func TestPruneEndedRejectsBadRequests(t *testing.T) {
	db := openDB(t, newDatabaseFile(t))
	rt := newRuntime(t, db, nil)
	insertStampedRun(t, db, stampedRun{key: "k-old", handler: "job", state: durable.RunSucceeded, at: pruneBase})
	cutoff := pruneBase.Add(time.Hour)
	cases := []struct {
		name string
		req  PruneRequest
	}{
		{"no handler", PruneRequest{Before: cutoff, Limit: 1}},
		{"empty handler list", PruneRequest{Handlers: []string{}, Before: cutoff, Limit: 1}},
		{"zero cutoff", PruneRequest{Handlers: []string{"job"}, Limit: 1}},
		{"zero limit", PruneRequest{Handlers: []string{"job"}, Before: cutoff}},
		{"negative limit", PruneRequest{Handlers: []string{"job"}, Before: cutoff, Limit: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, err := rt.PruneEnded(t.Context(), tc.req)
			requireCode(t, err, ErrorCodeInvalidArgument)
			if n != 0 {
				t.Fatalf("pruned %d runs on a bad request", n)
			}
		})
	}
	if got := runKeys(t, db); !slices.Equal(got, []string{"k-old"}) {
		t.Fatalf("runs = %q, want the old run kept", got)
	}
}
