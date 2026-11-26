package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/platform/turn/store"
	"github.com/anggasct/aura/internal/store/storetest"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	return storetest.OpenMigrated(t)
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

func wantCode(t *testing.T, err error, want ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with code %s, got nil", want)
	}
	got, ok := fault.CodeOf[ErrorCode](err)
	if !ok {
		t.Fatalf("error %v carries no storage code, want %s", err, want)
	}
	if got != want {
		t.Errorf("error code = %s, want %s", got, want)
	}
}

func wantTurnCode(t *testing.T, err error, want turnstore.ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with code %s, got nil", want)
	}
	got, ok := fault.CodeOf[turnstore.ErrorCode](err)
	if !ok {
		t.Fatalf("error %v carries no turnstore code, want %s", err, want)
	}
	if got != want {
		t.Errorf("error code = %s, want %s", got, want)
	}
}

func TestLinkMissingBlobIsTyped(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")

	artifacts := NewArtifactStore(db, t.TempDir(), DefaultArtifactQuotaBytes)
	wantCode(t, artifacts.Link(ctx, &ArtifactLink{
		ID: "link-1", BlobDigest: "missing-blob", SessionID: "session-1", Filename: "f.bin",
	}), ErrorCodeArtifactBlobMissing)
}

func TestLinkMissingSessionIsTyped(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	artifacts := NewArtifactStore(db, t.TempDir(), DefaultArtifactQuotaBytes)

	ref, err := artifacts.Put(ctx, strings.NewReader("payload"), &ArtifactMetadata{
		ID: "artifact-1", SessionID: "session-1", Filename: "f.bin", MediaType: "application/octet-stream",
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	wantCode(t, artifacts.Link(ctx, &ArtifactLink{
		ID: "link-1", BlobDigest: ref.BlobDigest, SessionID: "missing-session", Filename: "f.bin",
	}), ErrorCodeSessionNotFound)
}

func TestPutMissingSessionIsTyped(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	artifacts := NewArtifactStore(db, t.TempDir(), DefaultArtifactQuotaBytes)

	_, err := artifacts.Put(ctx, strings.NewReader("payload"), &ArtifactMetadata{
		ID: "artifact-1", SessionID: "missing-session", Filename: "f.bin", MediaType: "application/octet-stream",
	})
	wantCode(t, err, ErrorCodeSessionNotFound)
}

func TestPortsRejectNilArguments(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	root := t.TempDir()

	wantTurnCode(t, turnstore.NewSessionService(db).Create(ctx, nil), turnstore.ErrorCodeInvalidArgument)
	wantTurnCode(t, turnstore.NewEventStore(db).Append(ctx, nil), turnstore.ErrorCodeInvalidArgument)

	artifacts := NewArtifactStore(db, root, DefaultArtifactQuotaBytes)
	_, err := artifacts.Put(ctx, bytes.NewReader(nil), nil)
	wantCode(t, err, ErrorCodeInvalidArgument)
	wantCode(t, artifacts.Link(ctx, nil), ErrorCodeInvalidArgument)

	_, err = turnstore.RuntimeEventToADK(nil)
	wantTurnCode(t, err, turnstore.ErrorCodeInvalidArgument)

	_, err = turnstore.RuntimeEventFromADK("session-1", "turn-1", nil)
	wantTurnCode(t, err, turnstore.ErrorCodeInvalidArgument)
}

func TestArtifactOpenRejectsEscapingRelativePath(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	root := t.TempDir()
	mustCreateSession(t, db, "session-1")
	artifacts := NewArtifactStore(db, root, DefaultArtifactQuotaBytes)

	for i, rel := range []string{"../../etc/passwd", "a/../../b", "/etc/passwd", ".."} {
		ref, err := artifacts.Put(ctx, bytes.NewReader([]byte("content")), &ArtifactMetadata{
			ID: fmt.Sprintf("artifact-%d", i), SessionID: "session-1", Filename: "f.txt", MediaType: "text/plain",
		})
		if err != nil {
			t.Fatalf("Put %q: %v", rel, err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE blob SET relative_path = ? WHERE digest = ?`, rel, ref.BlobDigest); err != nil {
			t.Fatalf("corrupt relative_path: %v", err)
		}
		_, _, err = artifacts.Open(ctx, ref.ID)
		wantCode(t, err, ErrorCodeInvalidArgument)
	}
}

func TestVerifyRestoreRejectsEscapingManifestPath(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	destDir := filepath.Join(t.TempDir(), "backup")
	if _, err := Backup(ctx, db, destDir); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	hostile := BackupManifest{
		CreatedAt: time.Now().UTC(),
		Blobs:     []BackupBlobEntry{{Digest: "blob", SizeBytes: 1, RelativePath: "../../etc/passwd"}},
	}
	data, err := json.MarshalIndent(hostile, "", "  ")
	if err != nil {
		t.Fatalf("marshal hostile manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(destDir, backupManifestFilename), data, 0o600); err != nil {
		t.Fatalf("rewrite manifest: %v", err)
	}

	_, err = VerifyRestore(ctx, destDir, t.TempDir())
	wantCode(t, err, ErrorCodeInvalidArgument)
}

func TestArtifactPutRejectsOverQuotaDuringCopy(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	root := t.TempDir()
	mustCreateSession(t, db, "session-1")
	artifacts := NewArtifactStore(db, root, 4)

	_, err := artifacts.Put(ctx, bytes.NewReader([]byte("this is way more than four bytes")), &ArtifactMetadata{
		ID: "artifact-over-quota", SessionID: "session-1", Filename: "big.bin", MediaType: "application/octet-stream",
	})
	wantCode(t, err, ErrorCodeArtifactQuotaExceeded)

	var blobCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM blob`).Scan(&blobCount); err != nil {
		t.Fatalf("count blob rows: %v", err)
	}
	if blobCount != 0 {
		t.Errorf("blob rows = %d, want 0", blobCount)
	}
	entries, err := os.ReadDir(filepath.Join(root, "tmp"))
	if err != nil {
		t.Fatalf("read tmp dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("tmp dir holds %d leftover files, want 0", len(entries))
	}
}

func TestArtifactConcurrentSameDigestPutsDedupe(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	artifacts := NewArtifactStore(db, t.TempDir(), DefaultArtifactQuotaBytes)

	const workers = 8
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := artifacts.Put(ctx, bytes.NewReader([]byte("same bytes")), &ArtifactMetadata{
				ID: "artifact-dedupe", SessionID: "session-1", Filename: "f.bin", MediaType: "application/octet-stream",
			})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Put: %v", err)
		}
	}

	var blobCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM blob`).Scan(&blobCount); err != nil {
		t.Fatalf("count blob rows: %v", err)
	}
	if blobCount != 1 {
		t.Errorf("blob rows = %d, want 1 (single digest)", blobCount)
	}
}

func TestReconcileDetectsCorruptedBlobs(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	root := t.TempDir()
	artifacts := NewArtifactStore(db, root, DefaultArtifactQuotaBytes)

	ref, err := artifacts.Put(ctx, bytes.NewReader([]byte("original")), &ArtifactMetadata{
		ID: "artifact-1", SessionID: "session-1", Filename: "f.bin", MediaType: "application/octet-stream",
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	absPath := filepath.Join(root, "blobs", ref.BlobDigest[:2], ref.BlobDigest)
	if err := os.WriteFile(absPath, []byte("corrupted"), 0o600); err != nil {
		t.Fatalf("corrupt blob file: %v", err)
	}

	report, err := Reconcile(ctx, db, root)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(report.CorruptedBlobs) != 1 || report.CorruptedBlobs[0] != ref.BlobDigest {
		t.Errorf("CorruptedBlobs = %v, want [%s]", report.CorruptedBlobs, ref.BlobDigest)
	}
	if len(report.MissingBlobs) != 0 || len(report.OrphanFiles) != 0 {
		t.Errorf("unexpected missing=%v orphan=%v", report.MissingBlobs, report.OrphanFiles)
	}
}

func TestArtifactPutQuotaOverflowCannotBypassQuota(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	root := t.TempDir()
	mustCreateSession(t, db, "session-1")
	artifacts := NewArtifactStore(db, root, DefaultArtifactQuotaBytes)

	seed, err := artifacts.Put(ctx, bytes.NewReader([]byte("seed")), &ArtifactMetadata{
		ID: "artifact-seed", SessionID: "session-1", Filename: "s.txt", MediaType: "text/plain",
	})
	if err != nil {
		t.Fatalf("seed Put: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE blob SET size_bytes = ? WHERE digest = ?`, int64(math.MaxInt64), seed.BlobDigest); err != nil {
		t.Fatalf("corrupt size_bytes: %v", err)
	}

	_, err = artifacts.Put(ctx, bytes.NewReader([]byte("more")), &ArtifactMetadata{
		ID: "artifact-2", SessionID: "session-1", Filename: "m.txt", MediaType: "text/plain",
	})
	wantCode(t, err, ErrorCodeArtifactQuotaExceeded)
}

func TestArtifactAndBackupUseOwnerOnlyPermissions(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	root := t.TempDir()

	artifacts := NewArtifactStore(db, root, DefaultArtifactQuotaBytes)
	ref, err := artifacts.Put(ctx, bytes.NewReader([]byte("secret")), &ArtifactMetadata{
		ID:        "artifact-1",
		SessionID: "session-1",
		Filename:  "note.txt",
		MediaType: "text/plain",
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	blobDir := filepath.Join(root, "blobs", ref.BlobDigest[:2])
	assertMode(t, blobDir, 0o700)
	assertMode(t, filepath.Join(blobDir, ref.BlobDigest), 0o600)
	assertMode(t, filepath.Join(root, "tmp"), 0o700)

	destDir := filepath.Join(t.TempDir(), "backup")
	if _, err := Backup(ctx, db, destDir); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	assertMode(t, destDir, 0o700)
	assertMode(t, filepath.Join(destDir, backupManifestFilename), 0o600)
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode = %04o, want %04o", path, got, want)
	}
}
