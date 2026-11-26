package store

import (
	"database/sql"
	"testing"
)

func seedMemorySession(t *testing.T, db *sql.DB, sessionID, ownerID string) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `INSERT INTO session (id, owner_id, metadata_json, created_at, updated_at) VALUES (?,?,?,?,?) ON CONFLICT(id) DO NOTHING`,
		sessionID, ownerID, `{}`, "2026-09-12T12:00:00Z", "2026-09-12T12:00:00Z"); err != nil {
		t.Fatalf("seed session: %v", err)
	}
}
