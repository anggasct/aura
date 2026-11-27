package turnstore

import (
	"database/sql"
	"slices"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/turn"
)

func seedActivitySession(t *testing.T, db *sql.DB, id, ownerID, agentID string) {
	t.Helper()
	now := time.Now().UTC()
	sess := turn.Session{
		ID:        id,
		OwnerID:   ownerID,
		AgentID:   agentID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := NewTurnStore(db).CreateSession(t.Context(), &sess); err != nil {
		t.Fatalf("seed session %s: %v", id, err)
	}
}

func TestListPrincipalActivityAggregatesSessions(t *testing.T) {
	db := newTestDB(t)
	seedActivitySession(t, db, "kantor/discord:office/dm:1", "discord:1", "kantor")
	seedActivitySession(t, db, "kantor/discord:office/channel:9", "discord:1", "kantor")
	seedActivitySession(t, db, "main/discord:office/dm:777", "local-owner", "main")
	activity, err := NewTurnStore(db).PrincipalActivity(t.Context())
	if err != nil || len(activity) != 1 || activity[0].PrincipalID != "discord:1" || activity[0].Sessions != 2 || activity[0].LastActive.IsZero() {
		t.Fatalf("PrincipalActivity() = %+v, %v", activity, err)
	}
}

func TestRunSessionsAreNotPrincipalActivity(t *testing.T) {
	db := newTestDB(t)
	turns := NewTurnStore(db)
	seedActivitySession(t, db, "wf-run-only", "discord:2", "kantor")
	activity, err := turns.PrincipalActivity(t.Context())
	if err != nil || len(activity) != 0 {
		t.Fatalf("PrincipalActivity() with only a run session = %+v, %v; want none", activity, err)
	}
	seedActivitySession(t, db, "kantor/discord:office/dm:1", "discord:1", "kantor")
	if _, err := db.ExecContext(t.Context(), `UPDATE session SET updated_at = '2026-11-01T09:00:00Z' WHERE id = 'kantor/discord:office/dm:1'`); err != nil {
		t.Fatalf("age the chat session: %v", err)
	}
	seedActivitySession(t, db, "wf-run-later", "discord:1", "kantor")
	activity, err = turns.PrincipalActivity(t.Context())
	if err != nil || len(activity) != 1 {
		t.Fatalf("PrincipalActivity() = %+v, %v; want one principal", activity, err)
	}
	if activity[0].Sessions != 1 || activity[0].LastActive.Format("2006-01-02") != "2026-11-01" {
		t.Fatalf("activity = %+v, want the chat session only, last active 2026-11-01", activity[0])
	}
	seedActivitySession(t, db, "WF-chat", "discord:3", "kantor")
	activity, err = turns.PrincipalActivity(t.Context())
	if err != nil || len(activity) != 2 {
		t.Fatalf("PrincipalActivity() with an upper-case WF- chat session = %+v, %v; want two principals", activity, err)
	}
}

func TestFilterAndDeleteSessionsByIDScopeByOwner(t *testing.T) {
	db := newTestDB(t)
	turns := NewTurnStore(db)
	seedActivitySession(t, db, "wf-a", "workflow", "main")
	seedActivitySession(t, db, "wf-b", "workflow", "main")
	seedActivitySession(t, db, "wf-other", "discord:1", "main")

	empty, err := turns.FilterSessions(t.Context(), "workflow", nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("FilterSessions(nil) = %v, %v; want empty", empty, err)
	}
	found, err := turns.FilterSessions(t.Context(), "workflow", []string{"wf-a", "wf-other", "wf-missing"})
	if err != nil || !slices.Equal(found, []string{"wf-a"}) {
		t.Fatalf("FilterSessions = %v, %v; want [wf-a]", found, err)
	}
	if err := turns.DeleteSessionsByID(t.Context(), "workflow", nil); err != nil {
		t.Fatalf("DeleteSessionsByID(nil): %v", err)
	}
	if err := turns.DeleteSessionsByID(t.Context(), "workflow", []string{"wf-a", "wf-other"}); err != nil {
		t.Fatalf("DeleteSessionsByID: %v", err)
	}
	remainingWorkflow, err := turns.FilterSessions(t.Context(), "workflow", []string{"wf-a", "wf-b"})
	if err != nil || !slices.Equal(remainingWorkflow, []string{"wf-b"}) {
		t.Fatalf("remaining workflow sessions = %v, %v; want [wf-b]", remainingWorkflow, err)
	}
	remainingOther, err := turns.FilterSessions(t.Context(), "discord:1", []string{"wf-other"})
	if err != nil || !slices.Equal(remainingOther, []string{"wf-other"}) {
		t.Fatalf("other owner session was deleted: %v, %v", remainingOther, err)
	}
}
