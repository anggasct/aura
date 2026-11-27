package workflow

import (
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

func TestProposalPlanTextIsImmutable(t *testing.T) {
	ctx := t.Context()
	db := sqlitetest.OpenMigrated(t, turnstore.Schema(), Schema())
	seedMigrationSession(t, db, "sess-plan", "owner-1")
	if _, err := db.ExecContext(ctx, `INSERT INTO workflow_proposal (id, session_id, agent_id, principal_id, spec_json, spec_sha256, status, created_at, plan_text)
VALUES ('prop-plan', 'sess-plan', 'main', 'owner-1', '{}', 'digest', 'proposed', '2026-11-13T10:00:00Z', '1. approve')`); err != nil {
		t.Fatalf("seed proposal: %v", err)
	}
	_, err := db.ExecContext(ctx, `UPDATE workflow_proposal SET plan_text = '1. something else' WHERE id = 'prop-plan'`)
	if err == nil || !strings.Contains(err.Error(), "workflow_proposal.plan_text is immutable") {
		t.Fatalf("UPDATE plan_text = %v, want the immutability abort", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE workflow_proposal SET plan_text = '1. approve', status = 'approved' WHERE id = 'prop-plan'`); err != nil {
		t.Fatalf("UPDATE status with the same plan_text = %v, want success", err)
	}
	var plan, status string
	if err := db.QueryRowContext(ctx, `SELECT plan_text, status FROM workflow_proposal WHERE id = 'prop-plan'`).Scan(&plan, &status); err != nil {
		t.Fatal(err)
	}
	if plan != "1. approve" || status != "approved" {
		t.Fatalf("proposal = %q %q, want the original plan and the new status", plan, status)
	}
}
