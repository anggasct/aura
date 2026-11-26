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

func TestRuntimeRunStatesByKeysAndDeleteHandlerRunsScopeByHandler(t *testing.T) {
	db := openDB(t, newDatabaseFile(t))
	rt := newRuntime(t, db, nil)
	ctx := t.Context()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, row := range []struct {
		key, handler, state string
	}{
		{"wf-1", "workflow", "running"},
		{"wf-2", "workflow", "cancelled"},
		{"turn-1", "turn", "running"},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO durable_run (key, handler, payload, state, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
			row.key, row.handler, []byte(`{}`), row.state, now, now,
		); err != nil {
			t.Fatalf("seed run %s: %v", row.key, err)
		}
	}

	empty, err := rt.RunStatesByKeys(ctx, "workflow", nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("RunStatesByKeys(nil) = %v, %v; want empty", empty, err)
	}
	states, err := rt.RunStatesByKeys(ctx, "workflow", []string{"wf-1", "wf-2", "turn-1", "missing"})
	if err != nil {
		t.Fatalf("RunStatesByKeys: %v", err)
	}
	if len(states) != 2 || states["wf-1"] != "running" || states["wf-2"] != "cancelled" {
		t.Fatalf("RunStatesByKeys = %v, want wf-1=running, wf-2=cancelled", states)
	}
	if err := rt.DeleteHandlerRuns(ctx, "workflow", nil); err != nil {
		t.Fatalf("DeleteHandlerRuns(nil): %v", err)
	}
	if err := rt.DeleteHandlerRuns(ctx, "workflow", []string{"wf-1", "turn-1"}); err != nil {
		t.Fatalf("DeleteHandlerRuns: %v", err)
	}
	wfRemaining, err := rt.RunsOf(ctx, "workflow")
	if err != nil || len(wfRemaining) != 1 || wfRemaining[0].Key != "wf-2" {
		t.Fatalf("remaining workflow runs = %+v, %v; want [wf-2]", wfRemaining, err)
	}
	turnRemaining, err := rt.RunsOf(ctx, "turn")
	if err != nil || len(turnRemaining) != 1 || turnRemaining[0].Key != "turn-1" {
		t.Fatalf("turn run was deleted: %+v, %v", turnRemaining, err)
	}
}
