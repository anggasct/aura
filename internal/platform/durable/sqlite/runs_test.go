package sqlite

import (
	"slices"
	"testing"
	"time"
)

func TestRuntimeRunsOfListsEveryRunOfAHandler(t *testing.T) {
	db := openDB(t, newDatabaseFile(t))
	rt := newRuntime(t, db, nil)
	ctx := t.Context()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, row := range []struct {
		key, handler, payload, state string
	}{
		{"turn-b", "turn", `{"session_id":"s2"}`, "succeeded"},
		{"turn-a", "turn", `{"session_id":"s1"}`, "running"},
		{"wf-1", "workflow", `{"session_id":"s1"}`, "running"},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO durable_run (key, handler, payload, state, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
			row.key, row.handler, []byte(row.payload), row.state, now, now,
		); err != nil {
			t.Fatalf("seed run %s: %v", row.key, err)
		}
	}

	got, err := rt.RunsOf(ctx, "turn")
	if err != nil {
		t.Fatalf("RunsOf: %v", err)
	}
	if len(got) != 2 || got[0].Key != "turn-a" || string(got[0].Payload) != `{"session_id":"s1"}` || got[1].Key != "turn-b" {
		t.Fatalf("RunsOf(turn) = %+v, want turn-a and turn-b", got)
	}
}

func TestRuntimeDeleteRunsDeletesOnlyTheNamedKeys(t *testing.T) {
	db := openDB(t, newDatabaseFile(t))
	rt := newRuntime(t, db, nil)
	ctx := t.Context()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, key := range []string{"k1", "k2", "k3"} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO durable_run (key, handler, payload, state, created_at, updated_at) VALUES (?, 'turn', ?, 'failed', ?, ?)`,
			key, []byte(`{}`), now, now,
		); err != nil {
			t.Fatalf("seed run %s: %v", key, err)
		}
	}

	deleted, err := rt.DeleteRuns(ctx, nil)
	if err != nil || deleted != 0 {
		t.Fatalf("DeleteRuns(nil) = %d, %v; want 0, nil", deleted, err)
	}

	deleted, err = rt.DeleteRuns(ctx, []string{"k1", "k3"})
	if err != nil || deleted != 2 {
		t.Fatalf("DeleteRuns([k1 k3]) = %d, %v; want 2, nil", deleted, err)
	}
	remaining, err := rt.RunsOf(ctx, "turn")
	if err != nil {
		t.Fatalf("RunsOf: %v", err)
	}
	var keys []string
	for _, r := range remaining {
		keys = append(keys, r.Key)
	}
	if !slices.Equal(keys, []string{"k2"}) {
		t.Fatalf("remaining keys = %v, want [k2]", keys)
	}
}
