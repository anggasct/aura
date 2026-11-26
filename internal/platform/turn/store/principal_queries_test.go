package turnstore

import (
	"database/sql"
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
