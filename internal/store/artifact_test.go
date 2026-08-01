package store

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func newArtifactStore(t *testing.T, db *sql.DB, quotaBytes int64) ArtifactStore {
	t.Helper()
	return NewArtifactStore(db, t.TempDir(), quotaBytes)
}

func TestArtifactPutAndOpen(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	artifacts := newArtifactStore(t, db, DefaultArtifactQuotaBytes)

	content := []byte("hello artifact")
	ref, err := artifacts.Put(ctx, bytes.NewReader(content), ArtifactMetadata{
		ID:        "artifact-1",
		SessionID: "session-1",
		Filename:  "hello.txt",
		MediaType: "text/plain",
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if ref.SizeBytes != int64(len(content)) {
		t.Errorf("SizeBytes = %d, want %d", ref.SizeBytes, len(content))
	}

	r, meta, err := artifacts.Open(ctx, "artifact-1")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = r.Close() }()

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read artifact content: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("content = %q, want %q", got, content)
	}
	if meta.MediaType != "text/plain" || meta.SessionID != "session-1" || meta.Filename != "hello.txt" {
		t.Errorf("Open() metadata = %+v", meta)
	}
}

func TestArtifactPutDedupesAcrossSessions(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-a")
	mustCreateSession(t, db, "session-b")
	artifacts := newArtifactStore(t, db, DefaultArtifactQuotaBytes)

	content := []byte("shared bytes")
	refA, err := artifacts.Put(ctx, bytes.NewReader(content), ArtifactMetadata{
		ID: "artifact-a", SessionID: "session-a", Filename: "shared.bin", MediaType: "application/octet-stream",
	})
	if err != nil {
		t.Fatalf("Put a: %v", err)
	}
	refB, err := artifacts.Put(ctx, bytes.NewReader(content), ArtifactMetadata{
		ID: "artifact-b", SessionID: "session-b", Filename: "shared-copy.bin", MediaType: "application/octet-stream",
	})
	if err != nil {
		t.Fatalf("Put b: %v", err)
	}

	if refA.BlobDigest != refB.BlobDigest {
		t.Fatalf("digests differ: %s vs %s, want same blob reused", refA.BlobDigest, refB.BlobDigest)
	}

	var blobCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM blob WHERE digest = ?`, refA.BlobDigest).Scan(&blobCount); err != nil {
		t.Fatalf("count blob rows: %v", err)
	}
	if blobCount != 1 {
		t.Errorf("blob rows for digest = %d, want 1 (no duplicate bytes)", blobCount)
	}

	// Unlinking one session's reference must not affect the other's ownership.
	if err := artifacts.Unlink(ctx, "artifact-a"); err != nil {
		t.Fatalf("Unlink a: %v", err)
	}
	if _, _, err := artifacts.Open(ctx, "artifact-a"); err == nil {
		t.Error("Open(artifact-a) after Unlink: expected error, got nil")
	}
	rb, _, err := artifacts.Open(ctx, "artifact-b")
	if err != nil {
		t.Fatalf("Open(artifact-b) after unlinking a: %v", err)
	}
	_ = rb.Close()
}

func TestArtifactPutIsIdempotentByID(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	artifacts := newArtifactStore(t, db, DefaultArtifactQuotaBytes)

	meta := ArtifactMetadata{ID: "artifact-1", SessionID: "session-1", Filename: "f.txt", MediaType: "text/plain"}
	if _, err := artifacts.Put(ctx, bytes.NewReader([]byte("v1")), meta); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	if _, err := artifacts.Put(ctx, bytes.NewReader([]byte("v1")), meta); err != nil {
		t.Fatalf("repeat Put (idempotent): %v", err)
	}

	var refCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifact_ref WHERE id = ?`, meta.ID).Scan(&refCount); err != nil {
		t.Fatalf("count artifact_ref rows: %v", err)
	}
	if refCount != 1 {
		t.Errorf("artifact_ref rows = %d, want 1", refCount)
	}
}

func TestArtifactPutQuotaExceeded(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	artifacts := newArtifactStore(t, db, 4) // tiny quota

	_, err := artifacts.Put(ctx, bytes.NewReader([]byte("too big")), ArtifactMetadata{
		ID: "artifact-1", SessionID: "session-1", Filename: "f.bin", MediaType: "application/octet-stream",
	})
	if err == nil {
		t.Fatal("expected artifact_quota_exceeded, got nil")
	}
	code, ok := CodeOf(err)
	if !ok || code != ErrorCodeArtifactQuotaExceeded {
		t.Fatalf("CodeOf(err) = %v, %v; want %s", code, ok, ErrorCodeArtifactQuotaExceeded)
	}
}

func TestArtifactOpenMissingBlobFile(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	root := t.TempDir()
	artifacts := NewArtifactStore(db, root, DefaultArtifactQuotaBytes)

	ref, err := artifacts.Put(ctx, bytes.NewReader([]byte("bytes")), ArtifactMetadata{
		ID: "artifact-1", SessionID: "session-1", Filename: "f.bin", MediaType: "application/octet-stream",
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	absPath := filepath.Join(root, "blobs", ref.BlobDigest[:2], ref.BlobDigest)
	if err := os.Remove(absPath); err != nil {
		t.Fatalf("remove blob file: %v", err)
	}

	_, _, err = artifacts.Open(ctx, "artifact-1")
	if err == nil {
		t.Fatal("expected artifact_blob_missing, got nil")
	}
	code, ok := CodeOf(err)
	if !ok || code != ErrorCodeArtifactBlobMissing {
		t.Fatalf("CodeOf(err) = %v, %v; want %s", code, ok, ErrorCodeArtifactBlobMissing)
	}
}

func TestArtifactLink(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-a")
	mustCreateSession(t, db, "session-b")
	artifacts := newArtifactStore(t, db, DefaultArtifactQuotaBytes)

	content := []byte("linked bytes")
	ref, err := artifacts.Put(ctx, bytes.NewReader(content), ArtifactMetadata{
		ID: "artifact-a", SessionID: "session-a", Filename: "orig.bin", MediaType: "application/octet-stream",
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	if err := artifacts.Link(ctx, ArtifactLink{
		ID: "artifact-b", BlobDigest: ref.BlobDigest, SessionID: "session-b", Filename: "linked.bin",
	}); err != nil {
		t.Fatalf("Link: %v", err)
	}

	r, meta, err := artifacts.Open(ctx, "artifact-b")
	if err != nil {
		t.Fatalf("Open linked ref: %v", err)
	}
	defer func() { _ = r.Close() }()

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read linked content: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("linked content = %q, want %q", got, content)
	}
	if meta.SessionID != "session-b" || meta.Filename != "linked.bin" {
		t.Errorf("linked metadata = %+v", meta)
	}
}

func TestReconcileDetectsMissingAndOrphan(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	root := t.TempDir()
	artifacts := NewArtifactStore(db, root, DefaultArtifactQuotaBytes)

	ref, err := artifacts.Put(ctx, bytes.NewReader([]byte("tracked")), ArtifactMetadata{
		ID: "artifact-1", SessionID: "session-1", Filename: "f.bin", MediaType: "application/octet-stream",
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	missingPath := filepath.Join(root, "blobs", ref.BlobDigest[:2], ref.BlobDigest)
	if err := os.Remove(missingPath); err != nil {
		t.Fatalf("remove blob file: %v", err)
	}

	orphanDir := filepath.Join(root, "blobs", "zz")
	if err := os.MkdirAll(orphanDir, 0o755); err != nil {
		t.Fatalf("mkdir orphan dir: %v", err)
	}
	orphanPath := filepath.Join(orphanDir, "orphan-file")
	if err := os.WriteFile(orphanPath, []byte("untracked"), 0o644); err != nil {
		t.Fatalf("write orphan file: %v", err)
	}

	report, err := Reconcile(ctx, db, root)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(report.MissingBlobs) != 1 || report.MissingBlobs[0] != ref.BlobDigest {
		t.Errorf("MissingBlobs = %v, want [%s]", report.MissingBlobs, ref.BlobDigest)
	}
	wantOrphan := filepath.ToSlash(filepath.Join("blobs", "zz", "orphan-file"))
	if len(report.OrphanFiles) != 1 || report.OrphanFiles[0] != wantOrphan {
		t.Errorf("OrphanFiles = %v, want [%s]", report.OrphanFiles, wantOrphan)
	}
}
