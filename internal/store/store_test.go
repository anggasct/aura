package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/platform/turn/store"
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

func openDatabaseAtVersion(t *testing.T, path string, upTo int) *sql.DB {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, path, sqlite.Options{})
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db, history.Migrations()[:upTo]); err != nil {
		t.Fatalf("migrate to v%d: %v", upTo, err)
	}
	return db
}

func wantCode(t *testing.T, err error, want ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with code %s, got nil", want)
	}
	got, ok := fault.CodeOf[ErrorCode](err)
	if !ok {
		t.Fatalf("error %v carries no store code, want %s", err, want)
	}
	if got != want {
		t.Errorf("error code = %s, want %s", got, want)
	}
}

func newSession(id string) turn.Session {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return turn.Session{
		ID:        id,
		OwnerID:   "owner-1",
		CreatedAt: now,
		UpdatedAt: now,
		Metadata:  json.RawMessage(`{"k":"v"}`),
	}
}

func mustCreateSession(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	sess := newSession(id)
	if err := turnstore.NewSessionService(db).Create(context.Background(), &sess); err != nil {
		t.Fatalf("create session %s: %v", id, err)
	}
}
