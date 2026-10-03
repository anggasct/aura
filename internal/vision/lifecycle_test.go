package vision

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/store"
)

type bridgeStore struct {
	inner        store.ArtifactStore
	lastSourceID string
	puts         int
}

func (b *bridgeStore) Put(ctx context.Context, r io.Reader, meta *ArtifactMetadata) (ArtifactRef, error) {
	ref, err := b.inner.Put(ctx, r, &store.ArtifactMetadata{
		ID:        meta.ID,
		SessionID: meta.SessionID,
		EventID:   meta.EventID,
		Filename:  meta.Filename,
		MediaType: meta.MediaType,
		Metadata:  meta.Metadata,
	})
	if err != nil {
		return ArtifactRef{}, err
	}
	if b.puts == 0 {
		b.lastSourceID = ref.ID
	}
	b.puts++
	return ArtifactRef{ID: ref.ID, BlobDigest: ref.BlobDigest, SizeBytes: ref.SizeBytes}, nil
}

func (b *bridgeStore) Unlink(ctx context.Context, refID string) error {
	return b.inner.Unlink(ctx, refID)
}

func TestLifecycleUnlinkAndCollectLeavesNoOrphan(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dsn := filepath.Join(dir, "aura.db")
	db, err := store.OpenDB(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	root := filepath.Join(dir, "artifacts")
	now := time.Now().UTC()
	sessions := store.NewSessionService(db)
	if err := sessions.Create(ctx, &store.Session{ID: "sess-1", OwnerID: "owner", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	artifacts := store.NewArtifactStore(db, root, store.DefaultArtifactQuotaBytes)
	bridged := &bridgeStore{inner: artifacts}
	svc, err := NewService(ptrLimits(), WithStore(bridged))
	if err != nil {
		t.Fatal(err)
	}
	part, err := svc.Ingest(ctx, &IngestRequest{
		Content:    bytes.NewReader(pngBytes(t, 16, 16)),
		SessionID:  "sess-1",
		Provenance: Provenance{Source: "terminal", ExternalID: "line:1", TurnID: "turn-1"},
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if part.ArtifactID == "" {
		t.Fatal("empty derived id")
	}
	derivedID := part.ArtifactID
	sourceID := bridged.lastSourceID
	if sourceID == "" || sourceID == derivedID {
		t.Fatalf("source id missing or equal derived: %q vs %q", sourceID, derivedID)
	}
	if err := artifacts.Unlink(ctx, sourceID); err != nil {
		t.Fatalf("Unlink source: %v", err)
	}
	if sourceID != derivedID {
		if err := artifacts.Unlink(ctx, derivedID); err != nil {
			t.Fatalf("Unlink derived: %v", err)
		}
	}
	report, err := store.Collect(ctx, db, root, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if report.DeletedBlobs != 1 {
		t.Errorf("deleted blobs = %d, want 1 (png source and derived dedupe)", report.DeletedBlobs)
	}
	rep, err := store.Reconcile(ctx, db, root)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rep.MissingBlobs) != 0 || len(rep.OrphanFiles) != 0 || len(rep.CorruptedBlobs) != 0 {
		t.Errorf("reconcile after collect = %+v, want clean", rep)
	}
	entries, err := os.ReadDir(filepath.Join(root, "tmp"))
	if err == nil && len(entries) != 0 {
		t.Errorf("tmp leftovers = %d", len(entries))
	}
}

func TestLifecycleQuotaEnforcedDuringCopy(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dsn := filepath.Join(dir, "aura.db")
	db, err := store.OpenDB(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	root := filepath.Join(dir, "artifacts")
	now := time.Now().UTC()
	if err := store.NewSessionService(db).Create(ctx, &store.Session{ID: "sess-1", OwnerID: "owner", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	artifacts := store.NewArtifactStore(db, root, 16)
	bridged := &bridgeStore{inner: artifacts}
	svc, err := NewService(ptrLimits(), WithStore(bridged))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Ingest(ctx, &IngestRequest{
		Content:    bytes.NewReader(pngBytes(t, 32, 32)),
		SessionID:  "sess-1",
		Provenance: Provenance{Source: "terminal", ExternalID: "line:1", TurnID: "turn-1"},
	}); err == nil {
		t.Fatal("expected quota failure")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeArtifactQuotaExceeded {
		t.Fatalf("quota error code = %v, %v; want artifact_quota_exceeded", code, ok)
	}
}

func TestLifecycleBackupCarriesMetadataOnly(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dsn := filepath.Join(dir, "aura.db")
	db, err := store.OpenDB(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	root := filepath.Join(dir, "artifacts")
	now := time.Now().UTC()
	if err := store.NewSessionService(db).Create(ctx, &store.Session{ID: "sess-1", OwnerID: "owner", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	artifacts := store.NewArtifactStore(db, root, store.DefaultArtifactQuotaBytes)
	bridged := &bridgeStore{inner: artifacts}
	svc, err := NewService(ptrLimits(), WithStore(bridged))
	if err != nil {
		t.Fatal(err)
	}
	part, err := svc.Ingest(ctx, &IngestRequest{
		Content:    bytes.NewReader(pngBytes(t, 12, 12)),
		SessionID:  "sess-1",
		Provenance: Provenance{Source: "terminal", ExternalID: "line:1", TurnID: "turn-1"},
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	raw, err := MarshalPart(&part)
	if err != nil {
		t.Fatalf("MarshalPart: %v", err)
	}
	if strings.Contains(string(raw), "iVBOR") {
		t.Error("part embeds pixels")
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal part: %v", err)
	}
	for _, key := range []string{"bytes", "pixels", "base64", "path"} {
		if _, ok := decoded[key]; ok {
			t.Errorf("part carries %q", key)
		}
	}
	dest := filepath.Join(dir, "backup")
	manifest, err := store.Backup(ctx, db, dest)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if len(manifest.Blobs) != 1 {
		t.Errorf("backup blobs = %d, want 1 (png source and derived dedupe)", len(manifest.Blobs))
	}
}
