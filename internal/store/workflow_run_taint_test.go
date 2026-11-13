package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrationAddsRunTaintAndLineageWithoutDisturbingLegacyRows(t *testing.T) {
	ctx := t.Context()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, bootstrapSchemaMigrationTableSQL); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	for _, m := range migrations[:45] {
		if err := applyMigration(ctx, db, m); err != nil {
			t.Fatalf("apply v%d: %v", m.version, err)
		}
	}
	seedMemorySession(t, db, "sess-1", "owner-1")
	if _, err := db.ExecContext(ctx, `INSERT INTO workflow_definition (id, version, goal, source, spec_json, spec_sha256, created_at) VALUES ('flow', 1, 'g', 'defined', '{}', 'digest', '2026-11-13T10:00:00Z')`); err != nil {
		t.Fatalf("seed definition: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO workflow_run (id, definition_id, definition_version, durable_key, goal, input_json, status, created_at, updated_at, principal_id, principal_class, agent_id, origin_session_id)
VALUES ('run-legacy', 'flow', 1, 'dur-legacy', 'g', '{}', 'queued', '2026-11-13T10:00:00Z', '2026-11-13T10:00:00Z', 'discord:1', 'public', 'lobby', 'sess-1')`); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	insertProposal := func(id string, tainted any) error {
		_, err := db.ExecContext(ctx, `INSERT INTO workflow_proposal (id, session_id, agent_id, principal_id, spec_json, spec_sha256, status, created_at)
VALUES (?, 'sess-1', 'main', 'owner-1', '{}', 'digest', 'proposed', '2026-11-13T10:00:00Z')`, id)
		if err != nil || tainted == nil {
			return err
		}
		_, err = db.ExecContext(ctx, `UPDATE workflow_proposal SET tainted = ? WHERE id = ?`, tainted, id)
		return err
	}
	if err := insertProposal("prop-legacy", nil); err != nil {
		t.Fatalf("seed proposal: %v", err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	applied, latest, err := SchemaVersions(ctx, db)
	if err != nil || applied != 47 || latest != 47 {
		t.Fatalf("schema = applied %d latest %d %v, want 47", applied, latest, err)
	}
	var runTainted int
	var parent sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT tainted, parent_run_id FROM workflow_run WHERE id = 'run-legacy'`).Scan(&runTainted, &parent); err != nil {
		t.Fatalf("read legacy run: %v", err)
	}
	if runTainted != 0 || parent.Valid {
		t.Fatalf("legacy run tainted = %d parent valid = %v, want 0 and NULL", runTainted, parent.Valid)
	}
	var proposalTainted int
	if err := db.QueryRowContext(ctx, `SELECT tainted FROM workflow_proposal WHERE id = 'prop-legacy'`).Scan(&proposalTainted); err != nil || proposalTainted != 0 {
		t.Fatalf("legacy proposal tainted = %d, %v, want 0", proposalTainted, err)
	}
	if err := insertProposal("prop-tainted", 1); err != nil {
		t.Fatalf("a tainted proposal was refused: %v", err)
	}
	if err := insertProposal("prop-bad", 2); err == nil {
		t.Fatal("workflow_proposal.tainted accepted a value outside 0 and 1")
	}
	insertRun := func(id string, tainted int, parentID any) error {
		_, err := db.ExecContext(ctx, `INSERT INTO workflow_run (id, definition_id, definition_version, durable_key, goal, input_json, status, created_at, updated_at, tainted, parent_run_id)
VALUES (?, 'flow', 1, ?, 'g', '{}', 'queued', '2026-11-13T10:00:00Z', '2026-11-13T10:00:00Z', ?, ?)`, id, "dur-"+id, tainted, parentID)
		return err
	}
	if err := insertRun("run-child", 1, "run-legacy"); err != nil {
		t.Fatalf("a tainted child run was refused: %v", err)
	}
	if err := insertRun("run-bad-taint", 2, nil); err == nil {
		t.Fatal("workflow_run.tainted accepted a value outside 0 and 1")
	}
	if err := insertRun("run-orphan", 0, "run-missing"); err == nil {
		t.Fatal("workflow_run.parent_run_id accepted an unknown run")
	}
	var predicate string
	if err := db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'workflow_run_completion_idx'`).Scan(&predicate); err != nil {
		t.Fatalf("read completion index: %v", err)
	}
	for _, want := range []string{"origin_session_id IS NOT NULL", "completion_notified_at IS NULL", "parent_run_id IS NULL"} {
		if !strings.Contains(predicate, want) {
			t.Fatalf("completion index = %q, want predicate %q", predicate, want)
		}
	}
}
