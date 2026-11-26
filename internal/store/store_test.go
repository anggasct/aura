package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/store/history"
)

var migratedTemplate = sync.OnceValues(buildMigratedTemplate)

func buildMigratedTemplate() ([]byte, error) {
	dir, err := os.MkdirTemp("", "aura-store-template-")
	if err != nil {
		return nil, fmt.Errorf("create template directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	ctx := context.Background()
	source, err := sqlite.Open(ctx, filepath.Join(dir, "source.db"), sqlite.Options{})
	if err != nil {
		return nil, fmt.Errorf("open source database: %w", err)
	}
	defer func() { _ = source.Close() }()
	if err := sqlite.Migrate(ctx, source, history.Migrations()); err != nil {
		return nil, fmt.Errorf("migrate source database: %w", err)
	}
	snapshot := filepath.Join(dir, "template.db")
	if _, err := source.ExecContext(ctx, "VACUUM INTO ?", snapshot); err != nil {
		return nil, fmt.Errorf("snapshot migrated database: %w", err)
	}
	return os.ReadFile(snapshot)
}

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	template, err := migratedTemplate()
	if err != nil {
		t.Fatalf("build migrated template: %v", err)
	}
	dsn := filepath.Join(t.TempDir(), "aura.db")
	if err := os.WriteFile(dsn, template, 0o600); err != nil {
		t.Fatalf("seed database: %v", err)
	}
	db, err := sqlite.Open(t.Context(), dsn, sqlite.Options{})
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
