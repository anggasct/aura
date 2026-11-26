package runtimeengine

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"strconv"
	"testing"
	"time"

	durablesqlite "github.com/anggasct/aura/internal/platform/durable/sqlite"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/platform/turn/store"
	"github.com/anggasct/aura/internal/store/storetest"
)

type orphanRunFixture struct {
	key     string
	handler string
	payload string
	state   string
	actions int
}

type orphanEventFixture struct {
	session string
	turn    string
	kind    string
}

func seedOrphanSession(t *testing.T, db *sql.DB, id, ownerID, agentID string) {
	t.Helper()
	now := time.Now().UTC()
	if err := turnstore.NewSessionService(db).Create(t.Context(), &turn.Session{
		ID: id, OwnerID: ownerID, AgentID: agentID, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed session %s: %v", id, err)
	}
}

func seedOrphanRun(t *testing.T, db *sql.DB, run orphanRunFixture) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO durable_run (key, handler, payload, state, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		run.key, run.handler, []byte(run.payload), run.state, now, now,
	); err != nil {
		t.Fatalf("seed durable run %s: %v", run.key, err)
	}
	for i := range run.actions {
		if _, err := db.ExecContext(t.Context(),
			`INSERT INTO durable_action (run_key, action_key, outcome, result, created_at) VALUES (?, ?, 'result', ?, ?)`,
			run.key, "step-"+strconv.Itoa(i), []byte(`{"text":"model reply"}`), now,
		); err != nil {
			t.Fatalf("seed durable action %s/%d: %v", run.key, i, err)
		}
	}
}

func seedOrphanEvent(t *testing.T, db *sql.DB, ev orphanEventFixture) {
	t.Helper()
	event := turn.Event{
		ID:            ev.session + "-" + ev.turn + "-" + ev.kind,
		SessionID:     ev.session,
		TurnID:        ev.turn,
		InvocationID:  "inv-1",
		Author:        "user",
		Kind:          ev.kind,
		SchemaVersion: 1,
		Payload:       json.RawMessage(`{}`),
		CreatedAt:     time.Now().UTC(),
	}
	if _, err := turnstore.NewEventStore(db).AppendSequenced(t.Context(), ev.session, &event); err != nil {
		t.Fatalf("seed event %s: %v", event.ID, err)
	}
}

func countOrphanRows(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(t.Context(), query).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", query, err)
	}
	return n
}

func newOrphanRuntime(t *testing.T, db *sql.DB) *durablesqlite.Runtime {
	t.Helper()
	rt, err := durablesqlite.New(&durablesqlite.Options{DB: db, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("durablesqlite.New: %v", err)
	}
	return rt
}

func TestDeleteOrphanedTurnRuns(t *testing.T) {
	const (
		liveSession     = "lobby/discord:owner/dm:601"
		returnedSession = "lobby/discord:owner/dm:600"
		goneSession     = "lobby/discord:owner/dm:599"
	)
	db := storetest.OpenMigrated(t)
	runs := newOrphanRuntime(t, db)
	turns := turnstore.NewTurnStore(db)
	seedOrphanSession(t, db, liveSession, "discord:601", "lobby")
	seedOrphanSession(t, db, returnedSession, "discord:600", "lobby")
	for _, run := range []orphanRunFixture{
		{key: "turn-gone-running", handler: "turn", payload: `{"turn_id":"turn-gone-running","session_id":"` + goneSession + `"}`, state: "running", actions: 2},
		{key: "turn-returned-running", handler: "turn", payload: `{"turn_id":"turn-returned-running","session_id":"` + returnedSession + `"}`, state: "running", actions: 1},
		{key: "turn-returned-failed", handler: "turn", payload: `{"turn_id":"turn-returned-failed","session_id":"` + returnedSession + `"}`, state: "failed", actions: 1},
		{key: "turn-live", handler: "turn", payload: `{"turn_id":"turn-live","session_id":"` + liveSession + `"}`, state: "succeeded", actions: 1},
		{key: "workflow-gone", handler: "workflow", payload: `{"session_id":"` + goneSession + `"}`, state: "succeeded", actions: 1},
		{key: "turn-not-json", handler: "turn", payload: `not json`, state: "failed", actions: 1},
		{key: "turn-no-session", handler: "turn", payload: `{"turn_id":"turn-no-session"}`, state: "failed", actions: 1},
	} {
		seedOrphanRun(t, db, run)
	}
	for _, event := range []orphanEventFixture{
		{session: liveSession, turn: "turn-live", kind: "turn.accepted"},
		{session: liveSession, turn: "turn-returned-running", kind: "turn.accepted"},
		{session: returnedSession, turn: "turn-returned-failed", kind: "turn.cancelled"},
		{session: returnedSession, turn: "turn-returned-later", kind: "turn.accepted"},
	} {
		seedOrphanEvent(t, db, event)
	}
	deleted, err := DeleteOrphanedTurnRuns(t.Context(), db, runs, turns)
	if err != nil || deleted != 3 {
		t.Fatalf("DeleteOrphanedTurnRuns = %d, %v; want 3", deleted, err)
	}
	orphans := `('turn-gone-running', 'turn-returned-running', 'turn-returned-failed')`
	kept := `('turn-live', 'workflow-gone', 'turn-not-json', 'turn-no-session')`
	for query, expect := range map[string]int{
		`SELECT COUNT(*) FROM durable_run WHERE key IN ` + orphans:        0,
		`SELECT COUNT(*) FROM durable_action WHERE run_key IN ` + orphans: 0,
		`SELECT COUNT(*) FROM durable_run WHERE key IN ` + kept:           4,
		`SELECT COUNT(*) FROM durable_action WHERE run_key IN ` + kept:    4,
		`SELECT COUNT(*) FROM durable_run`:                                4,
	} {
		if got := countOrphanRows(t, db, query); got != expect {
			t.Errorf("%s = %d, want %d", query, got, expect)
		}
	}
	if again, err := DeleteOrphanedTurnRuns(t.Context(), db, runs, turns); err != nil || again != 0 {
		t.Errorf("second DeleteOrphanedTurnRuns = %d, %v; want 0", again, err)
	}
	for _, tc := range []struct {
		name  string
		db    *sql.DB
		runs  TurnRunStore
		turns AcceptedTurnReader
	}{
		{name: "nil database", runs: runs, turns: turns},
		{name: "nil runs", db: db, turns: turns},
		{name: "nil turns", db: db, runs: runs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DeleteOrphanedTurnRuns(t.Context(), tc.db, tc.runs, tc.turns); !fault.HasCode(err, turn.ErrorCodeInvalidArgument) {
				t.Errorf("DeleteOrphanedTurnRuns(%s) = %v, want %s", tc.name, err, turn.ErrorCodeInvalidArgument)
			}
		})
	}
}

func TestOrphanedTurnRunPayloadsDecodeAsTheSQLRuleDid(t *testing.T) {
	db := storetest.OpenMigrated(t)
	runs := newOrphanRuntime(t, db)
	turns := turnstore.NewTurnStore(db)
	seedOrphanSession(t, db, "sess-a", "discord:1", "lobby")
	seedOrphanSession(t, db, "sess-b", "discord:2", "lobby")

	for _, run := range []orphanRunFixture{
		{key: "turn-invalid-json", handler: "turn", payload: `{broken`, state: "failed"},
		{key: "turn-missing-session", handler: "turn", payload: `{"turn_id":"turn-missing-session"}`, state: "failed"},
		{key: "turn-null-session", handler: "turn", payload: `{"session_id":null}`, state: "failed"},
		{key: "turn-empty-session", handler: "turn", payload: `{"session_id":""}`, state: "failed"},
		{key: "turn-number-session", handler: "turn", payload: `{"session_id":42}`, state: "failed"},
		{key: "turn-accepted-same", handler: "turn", payload: `{"session_id":"sess-a"}`, state: "succeeded"},
		{key: "turn-accepted-other", handler: "turn", payload: `{"session_id":"sess-a"}`, state: "succeeded"},
	} {
		seedOrphanRun(t, db, run)
	}
	seedOrphanEvent(t, db, orphanEventFixture{session: "sess-a", turn: "turn-accepted-same", kind: turn.EventKindTurnAccepted})
	seedOrphanEvent(t, db, orphanEventFixture{session: "sess-b", turn: "turn-accepted-other", kind: turn.EventKindTurnAccepted})

	deleted, err := DeleteOrphanedTurnRuns(t.Context(), db, runs, turns)
	if err != nil || deleted != 3 {
		t.Fatalf("DeleteOrphanedTurnRuns = %d, %v; want 3", deleted, err)
	}
	for query, expect := range map[string]int{
		`SELECT COUNT(*) FROM durable_run WHERE key IN ('turn-invalid-json', 'turn-missing-session', 'turn-null-session', 'turn-accepted-same')`: 4,
		`SELECT COUNT(*) FROM durable_run WHERE key IN ('turn-empty-session', 'turn-number-session', 'turn-accepted-other')`:                     0,
	} {
		if got := countOrphanRows(t, db, query); got != expect {
			t.Errorf("%s = %d, want %d", query, got, expect)
		}
	}
}
