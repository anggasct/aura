package store

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupAndVerifyRestore(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")

	events := NewEventStore(db)
	e := newEvent("session-1", 1)
	if err := events.Append(ctx, &e); err != nil {
		t.Fatalf("Append: %v", err)
	}

	artifactRoot := t.TempDir()
	artifacts := NewArtifactStore(db, artifactRoot, DefaultArtifactQuotaBytes)
	if _, err := artifacts.Put(ctx, bytes.NewReader([]byte("backup me")), &ArtifactMetadata{
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
	ref, err := artifacts.Put(ctx, bytes.NewReader([]byte("original bytes")), &ArtifactMetadata{
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

func TestBackupExistingDestinationFailsCleanly(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")

	destDir := filepath.Join(t.TempDir(), "backup")
	if _, err := Backup(ctx, db, destDir); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	_, err := Backup(ctx, db, destDir)
	wantCode(t, err, ErrorCodeBackupDestinationConflict)
	if strings.Contains(err.Error(), destDir) {
		t.Errorf("conflict error leaks the full path: %v", err)
	}

	// The destination is not wedged: removing the snapshot lets the next
	// backup succeed, and no partial file was left behind.
	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatalf("read destDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("partial file left behind: %s", e.Name())
		}
	}
	if err := os.Remove(filepath.Join(destDir, backupDatabaseFilename)); err != nil {
		t.Fatalf("remove existing backup: %v", err)
	}
	if _, err := Backup(ctx, db, destDir); err != nil {
		t.Fatalf("Backup after cleanup: %v", err)
	}
}

func TestBackupFailedVACUUMLeavesNoPartial(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-based failure does not apply to root")
	}
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")

	destDir := filepath.Join(t.TempDir(), "backup")
	if err := os.MkdirAll(destDir, 0o500); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	defer func() { _ = os.Chmod(destDir, 0o700) }()

	_, err := Backup(ctx, db, destDir)
	if err == nil {
		t.Fatal("expected backup failure on a read-only directory, got nil")
	}
	if strings.Contains(err.Error(), destDir) {
		t.Errorf("error leaks the full path: %v", err)
	}

	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatalf("read destDir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("partial files left after failed backup: %v", names)
	}

	if err := os.Chmod(destDir, 0o700); err != nil {
		t.Fatalf("restore permissions: %v", err)
	}
	if _, err := Backup(ctx, db, destDir); err != nil {
		t.Fatalf("backup after a failed attempt must not be wedged: %v", err)
	}
}

func TestBackupManifestMatchesSnapshot(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	artifacts := NewArtifactStore(db, t.TempDir(), DefaultArtifactQuotaBytes)

	for i := range 2 {
		if _, err := artifacts.Put(ctx, bytes.NewReader([]byte(fmt.Sprintf("blob-%d", i))), &ArtifactMetadata{
			ID: fmt.Sprintf("artifact-%d", i), SessionID: "session-1", Filename: "f.bin", MediaType: "application/octet-stream",
		}); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}

	destDir := filepath.Join(t.TempDir(), "backup")
	manifest, err := Backup(ctx, db, destDir)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	snap, err := openReadOnly(ctx, filepath.Join(destDir, backupDatabaseFilename))
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer func() { _ = snap.Close() }()

	var inDB []string
	rows, err := snap.QueryContext(ctx, `SELECT digest FROM blob ORDER BY digest`)
	if err != nil {
		t.Fatalf("list snapshot blobs: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var digest string
		if err := rows.Scan(&digest); err != nil {
			t.Fatalf("scan snapshot blob: %v", err)
		}
		inDB = append(inDB, digest)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list snapshot blobs: %v", err)
	}

	if len(manifest.Blobs) != len(inDB) {
		t.Fatalf("manifest lists %d blobs, snapshot holds %d", len(manifest.Blobs), len(inDB))
	}
	for i, e := range manifest.Blobs {
		if e.Digest != inDB[i] {
			t.Errorf("manifest blob %d = %s, snapshot has %s", i, e.Digest, inDB[i])
		}
	}
}

func TestVerifyRestoreLeavesBackupReadOnly(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")

	destDir := filepath.Join(t.TempDir(), "backup")
	if _, err := Backup(ctx, db, destDir); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	if _, err := VerifyRestore(ctx, destDir, t.TempDir()); err != nil {
		t.Fatalf("VerifyRestore: %v", err)
	}

	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatalf("read backup dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), "-wal") || strings.HasSuffix(e.Name(), "-shm") {
			t.Errorf("verification mutated the backup: %s present", e.Name())
		}
	}
}
