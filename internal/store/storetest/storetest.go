package storetest

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/anggasct/aura/internal/store"
)

var migratedTemplate = sync.OnceValues(buildMigratedTemplate)

func Seed(t testing.TB, dbPath string) {
	t.Helper()
	template, err := migratedTemplate()
	if err != nil {
		t.Fatalf("build migrated template: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatalf("create database directory: %v", err)
	}
	file, err := os.OpenFile(dbPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatalf("create seeded database: %v", err)
	}
	if _, err := file.Write(template); err != nil {
		_ = file.Close()
		_ = os.Remove(dbPath)
		t.Fatalf("write seeded database: %v", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(dbPath)
		t.Fatalf("close seeded database: %v", err)
	}
}

func OpenMigrated(t testing.TB) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "aura.db")
	Seed(t, dbPath)
	db, err := store.OpenDB(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("open seeded database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func buildMigratedTemplate() ([]byte, error) {
	dir, err := os.MkdirTemp("", "aura-storetest-")
	if err != nil {
		return nil, fmt.Errorf("create template directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	ctx := context.Background()
	source, err := store.OpenDB(ctx, filepath.Join(dir, "source.db"))
	if err != nil {
		return nil, fmt.Errorf("open source database: %w", err)
	}
	defer func() { _ = source.Close() }()
	if err := store.Migrate(ctx, source); err != nil {
		return nil, fmt.Errorf("migrate source database: %w", err)
	}
	snapshot := filepath.Join(dir, "template.db")
	if _, err := source.ExecContext(ctx, "VACUUM INTO ?", snapshot); err != nil {
		return nil, fmt.Errorf("snapshot migrated database: %w", err)
	}
	data, err := os.ReadFile(snapshot)
	if err != nil {
		return nil, fmt.Errorf("read migrated snapshot: %w", err)
	}
	return data, nil
}
