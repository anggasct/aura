package store

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupAndVerifyRestore(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")

	events := NewEventStore(db)
	if err := events.Append(ctx, newEvent("session-1", 1)); err != nil {
		t.Fatalf("Append: %v", err)
	}

	artifactRoot := t.TempDir()
	artifacts := NewArtifactStore(db, artifactRoot, DefaultArtifactQuotaBytes)
	if _, err := artifacts.Put(ctx, bytes.NewReader([]byte("backup me")), ArtifactMetadata{
		ID: "artifact-1", SessionID: "session-1", Filename: "f.bin", MediaType: "application/octet-stream",
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	backupDir := filepath.Join(t.TempDir(), "backup")
	manifest, err := Backup(ctx, db, backupDir)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if len(manifest.Blobs) != 1 {
		t.Fatalf("manifest.Blobs = %d, want 1", len(manifest.Blobs))
	}

	report, err := VerifyRestore(ctx, backupDir, artifactRoot)
	if err != nil {
		t.Fatalf("VerifyRestore: %v", err)
	}
	if report.Sessions != 1 {
		t.Errorf("Sessions = %d, want 1", report.Sessions)
	}
	if report.Events != 1 {
		t.Errorf("Events = %d, want 1", report.Events)
	}
	if report.ArtifactRefs != 1 {
		t.Errorf("ArtifactRefs = %d, want 1", report.ArtifactRefs)
	}
	if report.VerifiedBlobs != 1 {
		t.Errorf("VerifiedBlobs = %d, want 1", report.VerifiedBlobs)
	}
	if len(report.ChecksumMismatches) != 0 || len(report.MissingBlobFiles) != 0 {
		t.Errorf("unexpected mismatches=%v missing=%v", report.ChecksumMismatches, report.MissingBlobFiles)
	}
}

func TestVerifyRestoreDetectsMissingAndCorruptedBlobs(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")

	artifactRoot := t.TempDir()
	artifacts := NewArtifactStore(db, artifactRoot, DefaultArtifactQuotaBytes)
	ref, err := artifacts.Put(ctx, bytes.NewReader([]byte("original bytes")), ArtifactMetadata{
		ID: "artifact-1", SessionID: "session-1", Filename: "f.bin", MediaType: "application/octet-stream",
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	backupDir := filepath.Join(t.TempDir(), "backup")
	if _, err := Backup(ctx, db, backupDir); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	absPath := filepath.Join(artifactRoot, "blobs", ref.BlobDigest[:2], ref.BlobDigest)
	if err := os.WriteFile(absPath, []byte("corrupted"), 0o644); err != nil {
		t.Fatalf("corrupt blob file: %v", err)
	}

	report, err := VerifyRestore(ctx, backupDir, artifactRoot)
	if err != nil {
		t.Fatalf("VerifyRestore: %v", err)
	}
	if len(report.ChecksumMismatches) != 1 || report.ChecksumMismatches[0] != ref.BlobDigest {
		t.Errorf("ChecksumMismatches = %v, want [%s]", report.ChecksumMismatches, ref.BlobDigest)
	}

	if err := os.Remove(absPath); err != nil {
		t.Fatalf("remove blob file: %v", err)
	}
	report, err = VerifyRestore(ctx, backupDir, artifactRoot)
	if err != nil {
		t.Fatalf("VerifyRestore: %v", err)
	}
	if len(report.MissingBlobFiles) != 1 || report.MissingBlobFiles[0] != ref.BlobDigest {
		t.Errorf("MissingBlobFiles = %v, want [%s]", report.MissingBlobFiles, ref.BlobDigest)
	}
}
