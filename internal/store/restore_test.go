package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustPutArtifact(t *testing.T, db *sql.DB, root, id, sessionID string, content []byte) ArtifactRef {
	t.Helper()
	artifacts := NewArtifactStore(db, root, DefaultArtifactQuotaBytes)
	ref, err := artifacts.Put(context.Background(), bytes.NewReader(content), &ArtifactMetadata{
		ID: id, SessionID: sessionID, Filename: "f.bin", MediaType: "application/octet-stream",
	})
	if err != nil {
		t.Fatalf("Put %s: %v", id, err)
	}
	return ref
}

func TestRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	e := newEvent("session-1", 1)
	if err := NewEventStore(db).Append(ctx, &e); err != nil {
		t.Fatalf("Append: %v", err)
	}
	artifactRoot := t.TempDir()
	mustPutArtifact(t, db, artifactRoot, "artifact-1", "session-1", []byte("restore me"))

	backupDir := filepath.Join(t.TempDir(), "backup")
	if _, err := Backup(ctx, db, backupDir); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	liveDir := t.TempDir()
	liveDB := filepath.Join(liveDir, "aura.db")
	report, err := Restore(ctx, backupDir, artifactRoot, liveDB, RestoreOptions{})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if report.Sessions != 1 || report.Events != 1 || report.VerifiedBlobs != 1 {
		t.Errorf("restore report = %+v, want 1 session, 1 event, 1 verified blob", report)
	}

	restored, err := OpenDB(ctx, liveDB)
	if err != nil {
		t.Fatalf("open restored db: %v", err)
	}
	defer func() { _ = restored.Close() }()
	if err := Migrate(ctx, restored); err != nil {
		t.Fatalf("Migrate restored db: %v", err)
	}
	sessions := NewSessionService(restored)
	got, err := sessions.Get(ctx, "session-1")
	if err != nil {
		t.Fatalf("Get restored session: %v", err)
	}
	if got.ID != "session-1" {
		t.Errorf("restored session = %+v", got)
	}
	events, err := sessions.ListEvents(ctx, "session-1", 0, 10)
	if err != nil || len(events) != 1 {
		t.Errorf("restored events = %d, err = %v; want 1", len(events), err)
	}

	info, err := os.Stat(liveDB)
	if err != nil {
		t.Fatalf("stat restored db: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("restored db perm = %o, want 600", perm)
	}
}

func TestRestoreRefusesExistingLiveDBWithoutForce(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	backupDir := filepath.Join(t.TempDir(), "backup")
	if _, err := Backup(ctx, db, backupDir); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	liveDB := filepath.Join(t.TempDir(), "aura.db")
	if err := os.WriteFile(liveDB, []byte("existing"), 0o600); err != nil {
		t.Fatalf("write existing live db: %v", err)
	}
	_, err := Restore(ctx, backupDir, t.TempDir(), liveDB, RestoreOptions{})
	wantCode(t, err, ErrorCodeRestoreLocked)
	if _, ok := CodeOf(err); !ok {
		t.Fatalf("restore_locked must be a typed error, got %v", err)
	}

	data, err := os.ReadFile(liveDB)
	if err != nil {
		t.Fatalf("read live db: %v", err)
	}
	if string(data) != "existing" {
		t.Errorf("live db was modified by a refused restore: %q", data)
	}
}

func TestRestoreForceReplacesLiveDB(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	backupDir := filepath.Join(t.TempDir(), "backup")
	if _, err := Backup(ctx, db, backupDir); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	liveDB := filepath.Join(t.TempDir(), "aura.db")
	if err := os.WriteFile(liveDB, []byte("existing"), 0o600); err != nil {
		t.Fatalf("write existing live db: %v", err)
	}
	if _, err := Restore(ctx, backupDir, t.TempDir(), liveDB, RestoreOptions{Force: true}); err != nil {
		t.Fatalf("Restore with force: %v", err)
	}

	restored, err := OpenDB(ctx, liveDB)
	if err != nil {
		t.Fatalf("open restored db: %v", err)
	}
	defer func() { _ = restored.Close() }()
	if err := Migrate(ctx, restored); err != nil {
		t.Fatalf("Migrate restored db: %v", err)
	}
	if _, err := NewSessionService(restored).Get(ctx, "session-1"); err != nil {
		t.Errorf("restored session missing: %v", err)
	}
}

func TestRestoreRejectsCorruptedBackup(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	artifactRoot := t.TempDir()
	ref := mustPutArtifact(t, db, artifactRoot, "artifact-1", "session-1", []byte("content"))

	backupDir := filepath.Join(t.TempDir(), "backup")
	if _, err := Backup(ctx, db, backupDir); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	absPath := filepath.Join(artifactRoot, "blobs", ref.BlobDigest[:2], ref.BlobDigest)
	if err := os.WriteFile(absPath, []byte("corrupted"), 0o644); err != nil {
		t.Fatalf("corrupt blob: %v", err)
	}

	liveDB := filepath.Join(t.TempDir(), "aura.db")
	_, err := Restore(ctx, backupDir, artifactRoot, liveDB, RestoreOptions{Force: true})
	wantCode(t, err, ErrorCodeBackupInvalid)
	if _, statErr := os.Stat(liveDB); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("corrupted backup must not install a live database (stat err = %v)", statErr)
	}
}

func TestCollectDeletesOnlyOldUnreferencedBlobs(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	root := t.TempDir()

	// A referenced blob is never collected regardless of age.
	referenced := mustPutArtifact(t, db, root, "artifact-keep", "session-1", []byte("keep"))

	// An unreferenced blob created before the cutoff is collected.
	oldDigest := strings.Repeat("a", 64)
	recentDigest := strings.Repeat("b", 64)
	if _, err := db.ExecContext(ctx, `INSERT INTO blob (digest, size_bytes, media_type, relative_path, created_at)
		VALUES (?, ?, ?, ?, ?)`, oldDigest, 3, "text/plain", "blobs/aa/"+oldDigest, formatTime(time.Now().Add(-48*time.Hour))); err != nil {
		t.Fatalf("insert old unreferenced blob: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO blob (digest, size_bytes, media_type, relative_path, created_at)
		VALUES (?, ?, ?, ?, ?)`, recentDigest, 3, "text/plain", "blobs/bb/"+recentDigest, formatTime(time.Now().Add(-time.Hour))); err != nil {
		t.Fatalf("insert recent unreferenced blob: %v", err)
	}
	for _, digest := range []string{oldDigest, recentDigest} {
		abs := filepath.Join(root, "blobs", digest[:2], digest)
		if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
			t.Fatalf("mkdir blob dir: %v", err)
		}
		if err := os.WriteFile(abs, []byte("xyz"), 0o600); err != nil {
			t.Fatalf("write blob file: %v", err)
		}
	}

	report, err := Collect(ctx, db, root, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if report.DeletedBlobs != 1 || report.FreedBytes != 3 {
		t.Errorf("Collect report = %+v, want 1 blob / 3 bytes", report)
	}
	for _, digest := range []string{oldDigest, recentDigest} {
		abs := filepath.Join(root, "blobs", digest[:2], digest)
		_, statErr := os.Stat(abs)
		if digest == oldDigest && statErr == nil {
			t.Errorf("old unreferenced blob file %s still present", digest)
		}
		if digest == recentDigest && os.IsNotExist(statErr) {
			t.Errorf("recent unreferenced blob file %s must remain", digest)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "blobs", referenced.BlobDigest[:2], referenced.BlobDigest)); err != nil {
		t.Errorf("referenced blob file removed: %v", err)
	}

	rows, err := db.QueryContext(ctx, `SELECT COUNT(*) FROM blob`)
	if err != nil {
		t.Fatalf("count blobs: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var count int
	if !rows.Next() {
		t.Fatal("no count row")
	}
	if err := rows.Scan(&count); err != nil {
		t.Fatalf("scan count: %v", err)
	}
	if count != 2 {
		t.Errorf("blob rows after collect = %d, want 2 (referenced + recent unreferenced)", count)
	}
}

func TestCollectSkipsBlobReferencedAfterScan(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	root := t.TempDir()

	// The blob is old and unreferenced at scan time, so it is a candidate;
	// but a reference is linked before the conditional delete runs, which
	// must protect the row and the file.
	digest := strings.Repeat("a", 64)
	if _, err := db.ExecContext(ctx, `INSERT INTO blob (digest, size_bytes, media_type, relative_path, created_at)
		VALUES (?, ?, ?, ?, ?)`, digest, 3, "text/plain", "blobs/aa/"+digest, formatTime(time.Now().Add(-48*time.Hour))); err != nil {
		t.Fatalf("insert blob: %v", err)
	}
	abs := filepath.Join(root, "blobs", digest[:2], digest)
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(abs, []byte("xyz"), 0o600); err != nil {
		t.Fatalf("write blob file: %v", err)
	}

	report, err := Collect(ctx, db, root, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if report.DeletedBlobs != 1 {
		t.Fatalf("DeletedBlobs = %d, want 1 (blob unreferenced at scan time)", report.DeletedBlobs)
	}

	// Simulate the reference racing in after the candidate scan: re-insert
	// the blob plus a reference, then run Collect again; the blob must
	// survive because the conditional delete re-checks the reference.
	if _, err := db.ExecContext(ctx, `INSERT INTO blob (digest, size_bytes, media_type, relative_path, created_at)
		VALUES (?, ?, ?, ?, ?)`, digest, 3, "text/plain", "blobs/aa/"+digest, formatTime(time.Now().Add(-48*time.Hour))); err != nil {
		t.Fatalf("re-insert blob: %v", err)
	}
	if err := os.WriteFile(abs, []byte("xyz"), 0o600); err != nil {
		t.Fatalf("rewrite blob file: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO artifact_ref (id, blob_digest, session_id, filename, created_at)
		VALUES (?, ?, ?, ?, ?)`, "ref-1", digest, "session-1", "f.bin", formatTime(time.Now())); err != nil {
		t.Fatalf("insert artifact_ref: %v", err)
	}

	report, err = Collect(ctx, db, root, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("Collect with reference: %v", err)
	}
	if report.DeletedBlobs != 0 {
		t.Errorf("DeletedBlobs = %d, want 0 (blob became referenced before the delete)", report.DeletedBlobs)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Errorf("referenced blob file removed despite the re-check: %v", err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM blob WHERE digest = ?`, digest).Scan(&count); err != nil {
		t.Fatalf("count blob rows: %v", err)
	}
	if count != 1 {
		t.Errorf("blob rows = %d, want 1 (row must survive the conditional delete)", count)
	}
}

func TestBackupStoreInterfaceRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	artifactRoot := t.TempDir()
	liveDB := filepath.Join(t.TempDir(), "aura.db")

	backups := NewBackupStore(db, artifactRoot, liveDB)
	destDir := filepath.Join(t.TempDir(), "backup")
	created, err := backups.CreateBackup(ctx, destDir)
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	manifest, err := backups.VerifyBackup(ctx, destDir)
	if err != nil {
		t.Fatalf("VerifyBackup: %v", err)
	}
	if len(manifest.Blobs) != len(created.Blobs) {
		t.Errorf("verified manifest blobs = %d, want %d", len(manifest.Blobs), len(created.Blobs))
	}
	if err := backups.RestoreBackup(ctx, destDir, RestoreOptions{}); err != nil {
		t.Fatalf("RestoreBackup: %v", err)
	}
	if _, err := os.Stat(liveDB); err != nil {
		t.Errorf("restored live db missing: %v", err)
	}
}

func TestReconcilerCollect(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	root := t.TempDir()

	r := NewReconciler(db, root)
	report, err := r.Inspect(ctx)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if len(report.OrphanFiles) != 0 || len(report.MissingBlobs) != 0 {
		t.Errorf("inspect on empty root = %+v", report)
	}
	collected, err := r.Collect(ctx, time.Now())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if collected.DeletedBlobs != 0 {
		t.Errorf("collect on empty root deleted %d blobs", collected.DeletedBlobs)
	}
}
