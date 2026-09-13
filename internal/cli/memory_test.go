package cli

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/memory"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/telemetry"
)

func newMemoryTestDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	db, err := store.OpenDB(ctx, filepath.Join(t.TempDir(), "aura.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

func TestMemoryDocumentStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := newMemoryTestDB(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO session (id, owner_id, metadata_json, created_at, updated_at) VALUES (?,?,?,?,?)`,
		"sess-1", "owner-1", `{}`, "2026-09-13T00:00:00Z", "2026-09-13T00:00:00Z"); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	adapter := &memoryDocumentStore{store: store.NewMemoryStore(db)}
	now := time.Now().UTC().Truncate(time.Second)
	inserted, err := adapter.UpsertDocument(ctx, &memory.StoredDocument{
		ID: "mem_cli", OwnerID: "owner-1", SessionID: "sess-1", Kind: memory.KindEventText,
		FromSequence: 1, ToSequence: 1, Content: "the nightly backup finished",
		TrustLabel: "owner_input", PromptVersion: "", CreatedAt: now,
	})
	if err != nil || !inserted {
		t.Fatalf("UpsertDocument() = %v, %v", inserted, err)
	}
	hits, err := adapter.Search(ctx, &memory.StoredQuery{
		OwnerID: "owner-1", SessionID: "sess-1", Terms: "nightly backup", Limit: 10,
	})
	if err != nil {
		t.Fatalf("Search(): %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "mem_cli" || hits[0].TrustLabel != "owner_input" {
		t.Fatalf("hits = %+v", hits)
	}
	mark, found, err := adapter.ProjectionWatermark(ctx, "sess-1")
	if err != nil || !found || mark != 1 {
		t.Fatalf("watermark = %d, %v, %v", mark, found, err)
	}
	if err := adapter.DeleteSessionDocuments(ctx, "sess-1"); err != nil {
		t.Fatalf("DeleteSessionDocuments(): %v", err)
	}
}

func TestBuildMemoryProvider(t *testing.T) {
	db := newMemoryTestDB(t)
	cfg := config.Default()
	provider, err := buildMemoryProvider(&cfg, db, nil)
	if err != nil {
		t.Fatalf("buildMemoryProvider(): %v", err)
	}
	if provider == nil {
		t.Fatal("provider missing")
	}
	if observer := memoryRecorderObserver(nil); observer != nil {
		t.Error("nil recorder produced an observer")
	}
	recorder, err := telemetry.NewMemoryRecorder(nil)
	if err != nil {
		t.Fatalf("NewMemoryRecorder(): %v", err)
	}
	if observer := memoryRecorderObserver(recorder); observer == nil {
		t.Error("observer missing")
	}
}
