package app

import (
	"database/sql"
	"testing"

	"github.com/anggasct/aura/internal/platform/sqlite"
)

func openDatabaseFile(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	db, err := sqlite.Open(t.Context(), dbPath, sqlite.Options{})
	if err != nil {
		t.Fatalf("open the database file: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
