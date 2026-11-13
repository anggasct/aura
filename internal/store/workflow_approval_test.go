package store

import (
	"path/filepath"
	"testing"
)

func TestMigrationAddsWorkflowApprovalWithItsConstraints(t *testing.T) {
	ctx := t.Context()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "approval.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	applied, latest, err := SchemaVersions(ctx, db)
	if err != nil || applied != 47 || latest != 47 {
		t.Fatalf("schema = applied %d latest %d %v, want 47", applied, latest, err)
	}
	seedMemorySession(t, db, "sess-1", "owner-1")
	for version := 1; version <= 12; version++ {
		if _, err := db.ExecContext(ctx, `INSERT INTO workflow_definition (id, version, goal, source, spec_json, spec_sha256, created_at) VALUES ('flow', ?, 'g', 'generated', '{}', 'digest', '2026-11-13T10:00:00Z')`, version); err != nil {
			t.Fatalf("seed definition: %v", err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO workflow_proposal (id, session_id, agent_id, principal_id, spec_json, spec_sha256, status, created_at) VALUES ('prop-1', 'sess-1', 'main', 'owner-1', '{}', 'digest', 'proposed', '2026-11-13T10:00:00Z')`); err != nil {
		t.Fatalf("seed proposal: %v", err)
	}
	insert := func(definitionID string, version int, kind string, proposalID, sourceID, sourceVersion, tainted any) error {
		_, err := db.ExecContext(ctx, `INSERT INTO workflow_approval (definition_id, definition_version, spec_sha256, kind, agent_id, approver_id, proposal_id, source_definition_id, source_definition_version, tainted, approved_at)
VALUES (?, ?, 'digest', ?, 'main', 'owner-1', ?, ?, ?, ?, '2026-11-13T10:00:00Z')`, definitionID, version, kind, proposalID, sourceID, sourceVersion, tainted)
		return err
	}
	tests := []struct {
		name      string
		insert    func() error
		wantError bool
	}{
		{name: "installed record", insert: func() error { return insert("flow", 1, "installed", nil, nil, nil, 0) }},
		{name: "proposal record with its proposal", insert: func() error { return insert("flow", 2, "proposal", "prop-1", nil, nil, 1) }},
		{name: "routine record with its source", insert: func() error { return insert("flow", 3, "routine", nil, "flow", 1, 1) }},
		{name: "same definition version twice", insert: func() error { return insert("flow", 1, "installed", nil, nil, nil, 0) }, wantError: true},
		{name: "unknown definition", insert: func() error { return insert("ghost", 1, "installed", nil, nil, nil, 0) }, wantError: true},
		{name: "kind outside the three", insert: func() error { return insert("flow", 4, "imported", nil, nil, nil, 0) }, wantError: true},
		{name: "taint outside 0 and 1", insert: func() error { return insert("flow", 5, "installed", nil, nil, nil, 2) }, wantError: true},
		{name: "proposal record without a proposal", insert: func() error { return insert("flow", 6, "proposal", nil, nil, nil, 0) }, wantError: true},
		{name: "installed record with a proposal", insert: func() error { return insert("flow", 7, "installed", "prop-1", nil, nil, 0) }, wantError: true},
		{name: "proposal record of an unknown proposal", insert: func() error { return insert("flow", 8, "proposal", "prop-ghost", nil, nil, 0) }, wantError: true},
		{name: "routine record without a source", insert: func() error { return insert("flow", 9, "routine", nil, nil, nil, 0) }, wantError: true},
		{name: "routine record with half a source", insert: func() error { return insert("flow", 10, "routine", nil, "flow", nil, 0) }, wantError: true},
		{name: "installed record with a source", insert: func() error { return insert("flow", 11, "installed", nil, "flow", 1, 0) }, wantError: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.insert(); (err != nil) != tc.wantError {
				t.Fatalf("insert error = %v, want error = %v", err, tc.wantError)
			}
		})
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM workflow_definition WHERE id = 'flow' AND version = 1`); err == nil {
		t.Fatal("a definition with an approval record was deleted")
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM workflow_approval`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("approval records = %d, %v, want the three accepted ones", count, err)
	}
	var primaryKey int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('workflow_approval') WHERE pk > 0`).Scan(&primaryKey); err != nil || primaryKey != 2 {
		t.Fatalf("primary key columns = %d, %v, want definition_id and definition_version", primaryKey, err)
	}
}
