package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func testChildRun(id string) *ChildRun {
	now := time.Now().UTC().Truncate(time.Second)
	return &ChildRun{
		ID: id, IdempotencyKey: "key-" + id,
		ParentSessionID: "sess-parent", ParentTurnID: "turn-1", ParentInvocation: "inv-1",
		ChildSessionID: "sess-child-" + id, DurableKey: "child/" + id,
		ContextDigest: "digest-" + id, GrantsJSON: `[{"capability":"search"}]`,
		BudgetJSON: `{"max_tokens":1000}`, State: "queued",
		Deadline: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now,
	}
}

func seedChildSessions(t *testing.T, db *sql.DB, parent, childID string) {
	t.Helper()
	seedMemorySession(t, db, parent, "owner-1")
	seedMemorySession(t, db, childID, "owner-1")
}

func TestChildStore_RoundTrip(t *testing.T) {
	db := newTestDB(t)
	s := NewChildStore(db)
	ctx := t.Context()
	seedChildSessions(t, db, "sess-parent", "sess-child-ch-1")
	if err := s.InsertRun(ctx, testChildRun("ch-1")); err != nil {
		t.Fatalf("InsertRun: %v", err)
	}
	got, found, err := s.GetRun(ctx, "ch-1")
	if err != nil || !found {
		t.Fatalf("GetRun: %+v, %v, %v", got, found, err)
	}
	if got.DurableKey != "child/ch-1" || got.State != "queued" {
		t.Errorf("run = %+v", got)
	}
	if got.ContextDigest != "digest-ch-1" {
		t.Errorf("run depth/digest = %+v", got)
	}
	byKey, found, err := s.GetRunByInvocation(ctx, "inv-1", "key-ch-1")
	if err != nil || !found || byKey.ID != "ch-1" {
		t.Fatalf("GetRunByInvocation: %+v, %v, %v", byKey, found, err)
	}
	if err := s.SetState(ctx, "ch-1", "running", got.UpdatedAt.Add(time.Minute)); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	active, err := s.ActiveForParent(ctx, "sess-parent")
	if err != nil || len(active) != 1 {
		t.Fatalf("ActiveForParent: %+v, %v", active, err)
	}
}

func TestChildStore_Conflicts(t *testing.T) {
	db := newTestDB(t)
	s := NewChildStore(db)
	ctx := t.Context()
	seedChildSessions(t, db, "sess-parent", "sess-child-ch-1")
	if err := s.InsertRun(ctx, testChildRun("ch-1")); err != nil {
		t.Fatalf("InsertRun: %v", err)
	}
	duplicate := testChildRun("ch-1")
	if err := s.InsertRun(ctx, duplicate); err == nil {
		t.Fatal("duplicate id must conflict")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeChildConflict {
		t.Fatalf("code = %v, %v (%v)", code, ok, err)
	}
	sameSession := testChildRun("ch-2")
	sameSession.ChildSessionID = "sess-child-ch-1"
	if err := s.InsertRun(ctx, sameSession); err == nil {
		t.Fatal("duplicate child session must conflict")
	}
	altered := testChildRun("ch-3")
	altered.ParentInvocation = "inv-1"
	altered.IdempotencyKey = "key-ch-1"
	if err := s.InsertRun(ctx, altered); err == nil {
		t.Fatal("duplicate invocation key must conflict")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeChildConflict {
		t.Fatalf("code = %v, %v (%v)", code, ok, err)
	}
	if _, _, err := s.GetRun(ctx, "missing"); err != nil {
		t.Fatalf("GetRun missing: %v", err)
	}
	if err := s.SetState(ctx, "missing", "failed", time.Now().UTC()); err == nil {
		t.Fatal("SetState missing must fail")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeChildNotFound {
		t.Fatalf("code = %v, %v (%v)", code, ok, err)
	}
	if err := s.SetState(ctx, "ch-1", "bogus", time.Now().UTC()); err == nil {
		t.Fatal("SetState bogus must fail")
	}
	invalid := testChildRun("ch-9")
	invalid.State = "bogus"
	if err := s.InsertRun(ctx, invalid); err == nil {
		t.Fatal("bogus state must fail")
	}
	invalidGrants := testChildRun("ch-10")
	invalidGrants.GrantsJSON = "{nope"
	if err := s.InsertRun(ctx, invalidGrants); err == nil {
		t.Fatal("invalid grants JSON must fail")
	}
	emptyDigest := testChildRun("ch-11")
	emptyDigest.ChildSessionID = "sess-child-ch-11"
	seedMemorySession(t, db, "sess-child-ch-11", "owner-1")
	emptyDigest.ContextDigest = ""
	if err := s.InsertRun(ctx, emptyDigest); err == nil {
		t.Fatal("empty digest must fail")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
		t.Fatalf("code = %v, %v (%v)", code, ok, err)
	}
	blankDigest := testChildRun("ch-12")
	blankDigest.ChildSessionID = "sess-child-ch-12"
	seedMemorySession(t, db, "sess-child-ch-12", "owner-1")
	blankDigest.ContextDigest = "   "
	if err := s.InsertRun(ctx, blankDigest); err == nil {
		t.Fatal("whitespace digest must fail")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
		t.Fatalf("code = %v, %v (%v)", code, ok, err)
	}
}

func TestChildMigrationHasTable(t *testing.T) {
	db := newTestDB(t)
	for _, table := range []string{"child_run"} {
		var name string
		if err := db.QueryRowContext(t.Context(), `SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name); err != nil {
			t.Fatalf("table %s: %v", table, err)
		}
	}
}

func TestChildDepthRemovalUpgrade(t *testing.T) {
	ctx := t.Context()
	dsn := filepath.Join(t.TempDir(), "legacy.db")
	db, err := OpenDB(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, bootstrapSchemaMigrationTableSQL); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	for _, m := range migrations[:16] {
		if err := applyMigration(ctx, db, m); err != nil {
			t.Fatalf("apply v%d: %v", m.version, err)
		}
	}
	var depthCol string
	if err := db.QueryRowContext(ctx, `SELECT name FROM pragma_table_info('child_run') WHERE name = 'depth'`).Scan(&depthCol); err != nil {
		t.Fatalf("legacy depth column missing: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	seedMemorySession(t, db, "sess-parent", "owner-1")
	seedMemorySession(t, db, "sess-child-legacy", "owner-1")
	format := func(v time.Time) string { return v.UTC().Format(time.RFC3339Nano) }
	if _, err := db.ExecContext(ctx,
		`INSERT INTO child_run (id, idempotency_key, parent_session_id, parent_turn_id, parent_invocation_id, child_session_id, depth, durable_key, context_digest, grants_json, budget_json, state, deadline, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"ch-legacy", "key-legacy", "sess-parent", "turn-1", "inv-legacy", "sess-child-legacy", 1, "child/ch-legacy", "digest-legacy", `[]`, `{}`, "queued", format(now.Add(time.Hour)), format(now), format(now),
	); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate upgrade: %v", err)
	}
	applied, latest, err := SchemaVersions(ctx, db)
	if err != nil {
		t.Fatalf("SchemaVersions: %v", err)
	}
	if latest != 19 || applied != 19 {
		t.Fatalf("schema = applied %d latest %d, want 19", applied, latest)
	}
	if err := db.QueryRowContext(ctx, `SELECT name FROM pragma_table_info('child_run') WHERE name = 'depth'`).Scan(&depthCol); err == nil {
		t.Fatal("depth column must be removed by v17")
	}
	s := NewChildStore(db)
	got, found, err := s.GetRun(ctx, "ch-legacy")
	if err != nil || !found {
		t.Fatalf("GetRun legacy: %+v, %v, %v", got, found, err)
	}
	if got.DurableKey != "child/ch-legacy" {
		t.Fatalf("legacy row = %+v", got)
	}
	seedMemorySession(t, db, "sess-child-new", "owner-1")
	fresh := testChildRun("ch-new")
	fresh.ChildSessionID = "sess-child-new"
	if err := s.InsertRun(ctx, fresh); err != nil {
		t.Fatalf("InsertRun after upgrade: %v", err)
	}
}

func TestChildStore_SetResultRoundTrip(t *testing.T) {
	db := newTestDB(t)
	s := NewChildStore(db)
	ctx := t.Context()
	seedChildSessions(t, db, "sess-parent", "sess-child-ch-1")
	if err := s.InsertRun(ctx, testChildRun("ch-1")); err != nil {
		t.Fatalf("InsertRun: %v", err)
	}
	completed := time.Now().UTC().Truncate(time.Second)
	if err := s.SetResult(ctx, "ch-1", &ChildResult{
		Status: "completed", Output: "summary", ArtifactsJSON: `["a"]`,
		TokensUsed: 3, CostMicros: 1, CompletedAt: completed,
		Provenance: "child=ch-1 session=sess-child-ch-1 digest=digest-ch-1 durable=child/ch-1",
		ChildID: "ch-1", SessionID: "sess-child-ch-1", ContextDigest: "digest-ch-1", DurableKey: "child/ch-1",
		SourceRange: "inv-1", Model: "child-default", PromptVersion: "v1", Trust: "derived_untrusted",
	}, completed); err != nil {
		t.Fatalf("SetResult: %v", err)
	}
	got, found, err := s.GetRun(ctx, "ch-1")
	if err != nil || !found {
		t.Fatalf("GetRun: %+v, %v, %v", got, found, err)
	}
	if got.ResultStatus != "completed" || got.ResultOutput != "summary" || got.TokensUsed != 3 || got.CostMicros != 1 {
		t.Fatalf("persisted result = %+v", got)
	}
	if got.ResultProvenance == "" || got.CompletedAt.IsZero() {
		t.Fatalf("provenance and completion must persist: %+v", got)
	}
	if got.ResultChildID != "ch-1" || got.ResultTrust != "derived_untrusted" || got.ResultModel != "child-default" {
		t.Fatalf("typed provenance and trust must persist: %+v", got)
	}
	if err := s.SetResult(ctx, "missing", &ChildResult{Status: "completed", CompletedAt: completed}, completed); err == nil {
		t.Fatal("missing run must fail")
	}
	if err := s.SetResult(ctx, "ch-1", &ChildResult{Status: "completed", TokensUsed: -1, CompletedAt: completed}, completed); err == nil {
		t.Fatal("negative usage must fail")
	}
	if err := s.SetResult(ctx, "ch-1", &ChildResult{Status: "completed", Trust: "owner_input", CompletedAt: completed}, completed); err == nil {
		t.Fatal("trusted label must fail for untrusted child result")
	}
	if err := s.SetResult(ctx, "ch-1", nil, completed); err == nil {
		t.Fatal("nil result must fail")
	}
}

func TestChildResultUpgradePreservesRows(t *testing.T) {
	ctx := t.Context()
	dsn := filepath.Join(t.TempDir(), "result-legacy.db")
	db, err := OpenDB(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, bootstrapSchemaMigrationTableSQL); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	for _, m := range migrations[:17] {
		if err := applyMigration(ctx, db, m); err != nil {
			t.Fatalf("apply v%d: %v", m.version, err)
		}
	}
	now := time.Now().UTC().Truncate(time.Second)
	seedMemorySession(t, db, "sess-parent", "owner-1")
	seedMemorySession(t, db, "sess-child-legacy", "owner-1")
	format := func(v time.Time) string { return v.UTC().Format(time.RFC3339Nano) }
	if _, err := db.ExecContext(ctx,
		`INSERT INTO child_run (id, idempotency_key, parent_session_id, parent_turn_id, parent_invocation_id, child_session_id, durable_key, context_digest, grants_json, budget_json, state, deadline, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"ch-legacy", "key-legacy", "sess-parent", "turn-1", "inv-legacy", "sess-child-legacy", "child/ch-legacy", "digest-legacy", `[]`, `{}`, "queued", format(now.Add(time.Hour)), format(now), format(now),
	); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate upgrade: %v", err)
	}
	s := NewChildStore(db)
	got, found, err := s.GetRun(ctx, "ch-legacy")
	if err != nil || !found {
		t.Fatalf("GetRun legacy: %+v, %v, %v", got, found, err)
	}
	if got.ResultStatus != "" || got.TokensUsed != 0 {
		t.Fatalf("legacy result defaults = %+v", got)
	}
	completed := now.Add(time.Minute)
	if err := s.SetResult(ctx, "ch-legacy", &ChildResult{Status: "completed", Output: "ok", CompletedAt: completed, Provenance: "child=ch-legacy session=sess-child-legacy digest=digest-legacy durable=child/ch-legacy", ChildID: "ch-legacy", SessionID: "sess-child-legacy", ContextDigest: "digest-legacy", DurableKey: "child/ch-legacy", SourceRange: "inv-legacy", Model: "child-default", PromptVersion: "v1", Trust: "derived_untrusted"}, completed); err != nil {
		t.Fatalf("SetResult after upgrade: %v", err)
	}
	after, found, err := s.GetRun(ctx, "ch-legacy")
	if err != nil || !found {
		t.Fatalf("GetRun after upgrade: %+v, %v, %v", after, found, err)
	}
	if after.ResultTrust != "derived_untrusted" || after.ResultModel != "child-default" {
		t.Fatalf("typed upgrade must persist: %+v", after)
	}
}
