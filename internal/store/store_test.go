package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

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

func pragmaValue(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var v string
	if err := db.QueryRowContext(context.Background(), "PRAGMA "+name).Scan(&v); err != nil {
		t.Fatalf("pragma %s: %v", name, err)
	}
	return v
}

func TestArtifactPutMaxInt64Quota(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	artifacts := NewArtifactStore(db, t.TempDir(), math.MaxInt64)

	ref, err := artifacts.Put(ctx, bytes.NewReader([]byte("content")), &ArtifactMetadata{
		ID: "artifact-1", SessionID: "session-1", Filename: "f.bin", MediaType: "application/octet-stream",
	})
	if err != nil {
		t.Fatalf("Put with MaxInt64 quota: %v", err)
	}
	if ref.SizeBytes != int64(len("content")) {
		t.Errorf("SizeBytes = %d, want %d", ref.SizeBytes, len("content"))
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

func newEvent(sessionID string, sequence uint64) turn.Event {
	return turn.Event{
		ID:            fmt.Sprintf("%s-event-%d", sessionID, sequence),
		SessionID:     sessionID,
		Sequence:      sequence,
		TurnID:        "turn-1",
		InvocationID:  "invocation-1",
		Author:        "user",
		Kind:          "message",
		SchemaVersion: 1,
		Payload:       json.RawMessage(`{"text":"hi"}`),
		CreatedAt:     time.Now().UTC(),
	}
}

func mustCreateSession(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	sess := newSession(id)
	if err := turnstore.NewSessionService(db).Create(context.Background(), &sess); err != nil {
		t.Fatalf("create session %s: %v", id, err)
	}
}
