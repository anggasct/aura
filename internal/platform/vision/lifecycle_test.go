package vision_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/storage"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
	"github.com/anggasct/aura/internal/platform/vision"
	"github.com/anggasct/aura/internal/store/storetest"
)

func lifecycleLimits() *vision.Limits {
	return &vision.Limits{
		MaxEncodedBytes:      20971520,
		MaxPixels:            40000000,
		MaxDimension:         8192,
		DecodeTimeout:        5 * time.Second,
		MaxDecodeConcurrency: 4,
		MaxTransformMemory:   268435456,
		StripMetadata:        true,
		OrientNormalize:      true,
		TransformVersion:     "v1",
	}
}

func lifecyclePNGBytes(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.Set(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type bridgeStore struct {
	inner        vision.ArtifactWriter
	lastSourceID string
	puts         int
}

func (b *bridgeStore) Put(ctx context.Context, r io.Reader, meta *vision.ArtifactMetadata) (vision.ArtifactRef, error) {
	ref, err := b.inner.Put(ctx, r, meta)
	if err != nil {
		return vision.ArtifactRef{}, err
	}
	if b.puts == 0 {
		b.lastSourceID = ref.ID
	}
	b.puts++
	return ref, nil
}

func (b *bridgeStore) Unlink(ctx context.Context, refID string) error {
	return b.inner.Unlink(ctx, refID)
}

func TestLifecycleUnlinkAndCollectLeavesNoOrphan(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dsn := filepath.Join(dir, "aura.db")
	storetest.Seed(t, dsn)
	db, err := sqlite.Open(ctx, dsn, sqlite.Options{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	root := filepath.Join(dir, "artifacts")
	now := time.Now().UTC()
	sessions := turnstore.NewSessionService(db)
	if err := sessions.Create(ctx, &turn.Session{ID: "sess-1", OwnerID: "owner", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	artifacts := storage.NewArtifactStore(db, root, storage.DefaultArtifactQuotaBytes)
	bridged := &bridgeStore{inner: storage.NewVisionStore(artifacts)}
	svc, err := vision.NewService(lifecycleLimits(), vision.WithStore(bridged))
	if err != nil {
		t.Fatal(err)
	}
	part, err := svc.Ingest(ctx, &vision.IngestRequest{
		Content:    bytes.NewReader(lifecyclePNGBytes(t, 16, 16)),
		SessionID:  "sess-1",
		Provenance: vision.Provenance{Source: "terminal", ExternalID: "line:1", TurnID: "turn-1"},
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
	report, err := storage.Collect(ctx, db, root, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if report.DeletedBlobs != 1 {
		t.Errorf("deleted blobs = %d, want 1 (png source and derived dedupe)", report.DeletedBlobs)
	}
	rep, err := storage.Reconcile(ctx, db, root)
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
	storetest.Seed(t, dsn)
	db, err := sqlite.Open(ctx, dsn, sqlite.Options{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	root := filepath.Join(dir, "artifacts")
	now := time.Now().UTC()
	if err := turnstore.NewSessionService(db).Create(ctx, &turn.Session{ID: "sess-1", OwnerID: "owner", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	artifacts := storage.NewArtifactStore(db, root, 16)
	bridged := &bridgeStore{inner: storage.NewVisionStore(artifacts)}
	svc, err := vision.NewService(lifecycleLimits(), vision.WithStore(bridged))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Ingest(ctx, &vision.IngestRequest{
		Content:    bytes.NewReader(lifecyclePNGBytes(t, 32, 32)),
		SessionID:  "sess-1",
		Provenance: vision.Provenance{Source: "terminal", ExternalID: "line:1", TurnID: "turn-1"},
	}); err == nil {
		t.Fatal("expected quota failure")
	} else if !fault.HasCode(err, vision.ErrorCodeArtifactQuotaExceeded) {
		t.Fatalf("error = %v, want code %s", err, vision.ErrorCodeArtifactQuotaExceeded)
	}
}

func TestLifecycleBackupCarriesMetadataOnly(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dsn := filepath.Join(dir, "aura.db")
	storetest.Seed(t, dsn)
	db, err := sqlite.Open(ctx, dsn, sqlite.Options{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	root := filepath.Join(dir, "artifacts")
	now := time.Now().UTC()
	if err := turnstore.NewSessionService(db).Create(ctx, &turn.Session{ID: "sess-1", OwnerID: "owner", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	artifacts := storage.NewArtifactStore(db, root, storage.DefaultArtifactQuotaBytes)
	bridged := &bridgeStore{inner: storage.NewVisionStore(artifacts)}
	svc, err := vision.NewService(lifecycleLimits(), vision.WithStore(bridged))
	if err != nil {
		t.Fatal(err)
	}
	part, err := svc.Ingest(ctx, &vision.IngestRequest{
		Content:    bytes.NewReader(lifecyclePNGBytes(t, 12, 12)),
		SessionID:  "sess-1",
		Provenance: vision.Provenance{Source: "terminal", ExternalID: "line:1", TurnID: "turn-1"},
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	raw, err := vision.MarshalPart(&part)
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
	manifest, err := storage.Backup(ctx, db, dest)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if len(manifest.Blobs) != 1 {
		t.Errorf("backup blobs = %d, want 1 (png source and derived dedupe)", len(manifest.Blobs))
	}
}
