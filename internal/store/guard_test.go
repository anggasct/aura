package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func wantCode(t *testing.T, err error, want ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with code %s, got nil", want)
	}
	got, ok := CodeOf(err)
	if !ok {
		t.Fatalf("error %v carries no store code, want %s", err, want)
	}
	if got != want {
		t.Errorf("error code = %s, want %s", got, want)
	}
}

func TestAppendRejectsSequenceAboveInt64(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")

	e := newEvent("session-1", math.MaxInt64+1)
	wantCode(t, NewEventStore(db).Append(ctx, &e), ErrorCodeEventSequenceInvalid)

	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runtime_event`).Scan(&count); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if count != 0 {
		t.Errorf("rows written = %d, want 0 — rejection must happen before the exec", count)
	}
}

func TestAppendRejectsZeroSequence(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")

	e := newEvent("session-1", 0)
	wantCode(t, NewEventStore(db).Append(ctx, &e), ErrorCodeEventSequenceInvalid)
}

func TestAppendRejectsZeroSchemaVersion(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")

	e := newEvent("session-1", 1)
	e.SchemaVersion = 0
	wantCode(t, NewEventStore(db).Append(ctx, &e), ErrorCodeEventSchemaVersionInvalid)
}

func TestAppendMissingSessionIsTyped(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	e := newEvent("missing-session", 1)
	wantCode(t, NewEventStore(db).Append(ctx, &e), ErrorCodeSessionNotFound)
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

func TestAppendPreservesMaxValidSequence(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")

	e := newEvent("session-1", math.MaxInt64)
	if err := NewEventStore(db).Append(ctx, &e); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := NewEventStore(db).LastSequence(ctx, "session-1")
	if err != nil {
		t.Fatalf("LastSequence: %v", err)
	}
	if got != math.MaxInt64 {
		t.Errorf("LastSequence = %d, want %d", got, uint64(math.MaxInt64))
	}
}

// The schema CHECK constraint makes a negative sequence unreachable through
// SQL, so the read guard is exercised directly rather than through a row.
func TestSequenceConversionGuards(t *testing.T) {
	if _, err := sequenceToDB(math.MaxInt64 + 1); err == nil {
		t.Error("sequenceToDB accepted a value above MaxInt64")
	} else {
		wantCode(t, err, ErrorCodeEventSequenceInvalid)
	}
	if got, err := sequenceToDB(math.MaxInt64); err != nil || got != math.MaxInt64 {
		t.Errorf("sequenceToDB(MaxInt64) = %d, %v; want %d, nil", got, err, int64(math.MaxInt64))
	}

	if _, err := sequenceFromDB(-1); err == nil {
		t.Error("sequenceFromDB accepted a negative value")
	} else {
		wantCode(t, err, ErrorCodeEventSequenceInvalid)
	}
	if got, err := sequenceFromDB(42); err != nil || got != 42 {
		t.Errorf("sequenceFromDB(42) = %d, %v; want 42, nil", got, err)
	}

	for _, stored := range []int64{-1, math.MaxUint16 + 1} {
		if _, err := schemaVersionFromDB(stored); err == nil {
			t.Errorf("schemaVersionFromDB(%d) accepted an out-of-range value", stored)
		} else {
			wantCode(t, err, ErrorCodeEventSchemaVersionInvalid)
		}
	}
	if got, err := schemaVersionFromDB(math.MaxUint16); err != nil || got != math.MaxUint16 {
		t.Errorf("schemaVersionFromDB(MaxUint16) = %d, %v; want %d, nil", got, err, uint16(math.MaxUint16))
	}
}

func TestScanRejectsOutOfRangeSchemaVersion(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")

	e := newEvent("session-1", 1)
	if err := NewEventStore(db).Append(ctx, &e); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE runtime_event SET schema_version = ? WHERE id = ?`,
		int64(math.MaxUint16)+1, e.ID); err != nil {
		t.Fatalf("widen schema_version: %v", err)
	}

	_, err := NewSessionService(db).ListEvents(ctx, "session-1", 0, 10)
	wantCode(t, err, ErrorCodeEventSchemaVersionInvalid)
}

func TestPortsRejectNilArguments(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	root := t.TempDir()

	wantCode(t, NewSessionService(db).Create(ctx, nil), ErrorCodeInvalidArgument)
	wantCode(t, NewEventStore(db).Append(ctx, nil), ErrorCodeInvalidArgument)

	artifacts := NewArtifactStore(db, root, DefaultArtifactQuotaBytes)
	_, err := artifacts.Put(ctx, bytes.NewReader(nil), nil)
	wantCode(t, err, ErrorCodeInvalidArgument)
	wantCode(t, artifacts.Link(ctx, nil), ErrorCodeInvalidArgument)

	_, err = RuntimeEventToADK(nil)
	wantCode(t, err, ErrorCodeInvalidArgument)

	_, err = RuntimeEventFromADK("session-1", "turn-1", nil)
	wantCode(t, err, ErrorCodeInvalidArgument)
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

func TestSessionCreateDuplicateReturnsConflict(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")

	wantCode(t, NewSessionService(db).Create(ctx, &Session{ID: "session-1"}), ErrorCodeSessionIDConflict)
}

func TestListEventsRejectsNegativeLimit(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")

	_, err := NewSessionService(db).ListEvents(ctx, "session-1", 0, -1)
	wantCode(t, err, ErrorCodeInvalidArgument)
}

func TestEventAppendRejectsInvalidPayload(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")

	e := newEvent("session-1", 1)
	e.Payload = json.RawMessage(`{"broken":`)
	wantCode(t, NewEventStore(db).Append(ctx, &e), ErrorCodeEventPayloadInvalid)

	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runtime_event`).Scan(&count); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if count != 0 {
		t.Errorf("rows written = %d, want 0", count)
	}
}

func TestMigrateRejectsVersionAboveBinary(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")

	if _, err := db.ExecContext(ctx,
		`INSERT INTO schema_migration (version, checksum, applied_at) VALUES (?, ?, ?)`,
		migrations[len(migrations)-1].version+1, "future", "2026-08-01T00:00:00Z",
	); err != nil {
		t.Fatalf("insert future migration: %v", err)
	}

	wantCode(t, Migrate(ctx, db), ErrorCodeMigrationSequenceInvalid)
}

func TestValidateMigrationOrder(t *testing.T) {
	if err := validateMigrationOrder([]migration{{version: 1}, {version: 2}}); err != nil {
		t.Errorf("strictly increasing list rejected: %v", err)
	}
	for _, ms := range [][]migration{
		{{version: 1}, {version: 1}},
		{{version: 2}, {version: 1}},
	} {
		if err := validateMigrationOrder(ms); err == nil {
			t.Errorf("validateMigrationOrder(%+v) accepted a non-increasing list", ms)
		} else {
			wantCode(t, err, ErrorCodeMigrationSequenceInvalid)
		}
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
	// A wrapping quota check computes a negative total and lets the write
	// through; the guarded form must reject it.
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
