package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
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
