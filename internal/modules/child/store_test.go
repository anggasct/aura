package child

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

func testChildRun(id string) *Run {
	now := time.Now().UTC().Truncate(time.Second)
	return &Run{
		ID: id, IdempotencyKey: "key-" + id,
		ParentSessionID: "sess-parent", ParentTurnID: "turn-1", ParentInvocation: "inv-1",
		ChildSessionID: "sess-child-" + id, DurableKey: "child/" + id,
		ContextDigest: "digest-" + id, GrantsJSON: `[{"capability":"search"}]`,
		BudgetJSON: `{"max_tokens":1000}`, State: "queued",
		Deadline: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now,
	}
}

func seedLegacySession(t *testing.T, db *sql.DB, sessionID, ownerID string) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `INSERT INTO session (id, owner_id, metadata_json, created_at, updated_at) VALUES (?,?,?,?,?) ON CONFLICT(id) DO NOTHING`,
		sessionID, ownerID, `{}`, "2026-09-12T12:00:00Z", "2026-09-12T12:00:00Z"); err != nil {
		t.Fatalf("seed session: %v", err)
	}
}

func seedChildSessions(t *testing.T, db *sql.DB, parent, childID string) {
	t.Helper()
	seedLegacySession(t, db, parent, "owner-1")
	seedLegacySession(t, db, childID, "owner-1")
}

func newTestStore(t *testing.T) (*SQLiteStore, *sql.DB) {
	t.Helper()
	db := sqlitetest.OpenMigrated(t, turnstore.Schema(), Schema())
	s, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	return s, db
}

func TestNewSQLiteStoreRefusesNilDB(t *testing.T) {
	if _, err := NewSQLiteStore(nil); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("NewSQLiteStore(nil) = %v, want %s", err, ErrorCodeInvalidArgument)
	}
}

func TestChildStore_RoundTrip(t *testing.T) {
	s, db := newTestStore(t)
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

func TestChildStoreDepthAndByChildSession(t *testing.T) {
	s, db := newTestStore(t)
	ctx := t.Context()
	seedChildSessions(t, db, "sess-parent", "sess-child-ch-1")
	if err := s.InsertRun(ctx, testChildRun("ch-1")); err != nil {
		t.Fatalf("InsertRun: %v", err)
	}
	depth, err := s.Depth(ctx, "sess-parent")
	if err != nil || depth != 0 {
		t.Fatalf("Depth(parent) = %d, %v, want 0", depth, err)
	}
	depth, err = s.Depth(ctx, "sess-child-ch-1")
	if err != nil || depth != 1 {
		t.Fatalf("Depth(child) = %d, %v, want 1", depth, err)
	}
	if _, err := s.Depth(ctx, "   "); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("Depth(blank) = %v, want %s", err, ErrorCodeInvalidArgument)
	}
	bySession, found, err := s.GetByChildSession(ctx, "sess-child-ch-1")
	if err != nil || !found || bySession.ID != "ch-1" {
		t.Fatalf("GetByChildSession = %+v, %v, %v", bySession, found, err)
	}
	if _, found, err := s.GetByChildSession(ctx, "missing"); err != nil || found {
		t.Fatalf("GetByChildSession(missing) = %v, %v", found, err)
	}
}

func TestChildStoreJoinsCallersTransaction(t *testing.T) {
	s, db := newTestStore(t)
	ctx := t.Context()
	seedChildSessions(t, db, "sess-parent", "sess-child-ch-tx")
	sentinel := errors.New("abort caller transaction")

	err := sqlite.RunInTx(ctx, db, func(txCtx context.Context) error {
		if err := s.InsertRun(txCtx, testChildRun("ch-tx")); err != nil {
			return err
		}
		if err := s.SetState(txCtx, "ch-tx", StatusRunning, time.Now().UTC()); err != nil {
			return err
		}
		got, found, err := s.GetRun(txCtx, "ch-tx")
		if err != nil || !found || got.State != StatusRunning {
			t.Fatalf("GetRun inside tx = %+v, %v, %v", got, found, err)
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("RunInTx error = %v, want %v", err, sentinel)
	}
	if _, found, err := s.GetRun(ctx, "ch-tx"); err != nil || found {
		t.Fatalf("GetRun after rollback = %v, %v, want not found", found, err)
	}
}

func TestChildStore_Conflicts(t *testing.T) {
	s, db := newTestStore(t)
	ctx := t.Context()
	seedChildSessions(t, db, "sess-parent", "sess-child-ch-1")
	if err := s.InsertRun(ctx, testChildRun("ch-1")); err != nil {
		t.Fatalf("InsertRun: %v", err)
	}
	duplicate := testChildRun("ch-1")
	if err := s.InsertRun(ctx, duplicate); err == nil {
		t.Fatal("duplicate id must conflict")
	} else if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrorCodeChildConflict {
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
	} else if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrorCodeChildConflict {
		t.Fatalf("code = %v, %v (%v)", code, ok, err)
	}
	if _, _, err := s.GetRun(ctx, "missing"); err != nil {
		t.Fatalf("GetRun missing: %v", err)
	}
	if err := s.SetState(ctx, "missing", "failed", time.Now().UTC()); err == nil {
		t.Fatal("SetState missing must fail")
	} else if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrorCodeChildNotFound {
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
	seedLegacySession(t, db, "sess-child-ch-11", "owner-1")
	emptyDigest.ContextDigest = ""
	if err := s.InsertRun(ctx, emptyDigest); err == nil {
		t.Fatal("empty digest must fail")
	} else if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrorCodeInvalidArgument {
		t.Fatalf("code = %v, %v (%v)", code, ok, err)
	}
	blankDigest := testChildRun("ch-12")
	blankDigest.ChildSessionID = "sess-child-ch-12"
	seedLegacySession(t, db, "sess-child-ch-12", "owner-1")
	blankDigest.ContextDigest = "   "
	if err := s.InsertRun(ctx, blankDigest); err == nil {
		t.Fatal("whitespace digest must fail")
	} else if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrorCodeInvalidArgument {
		t.Fatalf("code = %v, %v (%v)", code, ok, err)
	}
}

func TestChildMigrationHasTable(t *testing.T) {
	db := sqlitetest.OpenMigrated(t, turnstore.Schema(), Schema())
	for _, table := range []string{"child_run"} {
		var name string
		if err := db.QueryRowContext(t.Context(), `SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name); err != nil {
			t.Fatalf("table %s: %v", table, err)
		}
	}
}

func TestChildBaselineSchemaOmitsDepthAndHasResultDefaults(t *testing.T) {
	ctx := t.Context()
	migrations, err := sqlite.Baseline(turnstore.Schema(), Schema())
	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	db := sqlitetest.OpenMigrations(t, migrations)
	applied, latest, err := sqlite.SchemaVersions(ctx, db, migrations)
	if err != nil || applied != 2 || latest != 2 {
		t.Fatalf("schema = applied %d latest %d, %v; want 2", applied, latest, err)
	}
	var depthCol string
	if err := db.QueryRowContext(ctx, `SELECT name FROM pragma_table_info('child_run') WHERE name = 'depth'`).Scan(&depthCol); err == nil {
		t.Fatal("depth column must not exist on child_run")
	}
	now := time.Now().UTC().Truncate(time.Second)
	seedLegacySession(t, db, "sess-parent", "owner-1")
	seedLegacySession(t, db, "sess-child-base", "owner-1")
	format := func(v time.Time) string { return v.UTC().Format(time.RFC3339Nano) }
	if _, err := db.ExecContext(ctx,
		`INSERT INTO child_run (id, idempotency_key, parent_session_id, parent_turn_id, parent_invocation_id, child_session_id, durable_key, context_digest, grants_json, budget_json, state, deadline, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"ch-base", "key-base", "sess-parent", "turn-1", "inv-base", "sess-child-base", "child/ch-base", "digest-base", `[]`, `{}`, "queued", format(now.Add(time.Hour)), format(now), format(now),
	); err != nil {
		t.Fatalf("insert baseline row: %v", err)
	}
	s, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	got, found, err := s.GetRun(ctx, "ch-base")
	if err != nil || !found {
		t.Fatalf("GetRun baseline: %+v, %v, %v", got, found, err)
	}
	if got.DurableKey != "child/ch-base" || got.ResultStatus != "" || got.TokensUsed != 0 || got.CostMicros != 0 {
		t.Fatalf("baseline row defaults = %+v", got)
	}
	completed := now.Add(time.Minute)
	if err := s.SetResult(ctx, "ch-base", &StoredResult{Status: "completed", Output: "ok", CompletedAt: completed, Provenance: "child=ch-base session=sess-child-base digest=digest-base durable=child/ch-base", ChildID: "ch-base", SessionID: "sess-child-base", ContextDigest: "digest-base", DurableKey: "child/ch-base", SourceRange: "inv-base", Model: "child-default", PromptVersion: "v1", Trust: "derived_untrusted"}, completed); err != nil {
		t.Fatalf("SetResult: %v", err)
	}
	after, found, err := s.GetRun(ctx, "ch-base")
	if err != nil || !found {
		t.Fatalf("GetRun after SetResult: %+v, %v, %v", after, found, err)
	}
	if after.ResultTrust != "derived_untrusted" || after.ResultModel != "child-default" {
		t.Fatalf("typed result must persist: %+v", after)
	}
	seedLegacySession(t, db, "sess-child-new", "owner-1")
	fresh := testChildRun("ch-new")
	fresh.ChildSessionID = "sess-child-new"
	if err := s.InsertRun(ctx, fresh); err != nil {
		t.Fatalf("InsertRun: %v", err)
	}
}

func TestChildStore_SetResultRoundTrip(t *testing.T) {
	s, db := newTestStore(t)
	ctx := t.Context()
	seedChildSessions(t, db, "sess-parent", "sess-child-ch-1")
	if err := s.InsertRun(ctx, testChildRun("ch-1")); err != nil {
		t.Fatalf("InsertRun: %v", err)
	}
	completed := time.Now().UTC().Truncate(time.Second)
	if err := s.SetResult(ctx, "ch-1", &StoredResult{
		Status: "completed", Output: "summary", ArtifactsJSON: `["a"]`,
		TokensUsed: 3, CostMicros: 1, CompletedAt: completed,
		Provenance: "child=ch-1 session=sess-child-ch-1 digest=digest-ch-1 durable=child/ch-1",
		ChildID:    "ch-1", SessionID: "sess-child-ch-1", ContextDigest: "digest-ch-1", DurableKey: "child/ch-1",
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
	if err := s.SetResult(ctx, "missing", &StoredResult{Status: "completed", CompletedAt: completed}, completed); err == nil {
		t.Fatal("missing run must fail")
	}
	if err := s.SetResult(ctx, "ch-1", &StoredResult{Status: "completed", TokensUsed: -1, CompletedAt: completed}, completed); err == nil {
		t.Fatal("negative usage must fail")
	}
	if err := s.SetResult(ctx, "ch-1", &StoredResult{Status: "completed", Trust: "owner_input", CompletedAt: completed}, completed); err == nil {
		t.Fatal("trusted label must fail for untrusted child result")
	}
	if err := s.SetResult(ctx, "ch-1", nil, completed); err == nil {
		t.Fatal("nil result must fail")
	}
}
