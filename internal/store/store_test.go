package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "aura.db")
	db, err := OpenDB(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

func pragmaValue(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var v string
	if err := db.QueryRowContext(context.Background(), "PRAGMA "+name).Scan(&v); err != nil {
		t.Fatalf("pragma %s: %v", name, err)
	}
	return v
}

func TestOpenDBAppliesConnectionPolicy(t *testing.T) {
	db := newTestDB(t)

	if got := pragmaValue(t, db, "foreign_keys"); got != "1" {
		t.Errorf("foreign_keys = %s, want 1", got)
	}
	if got := pragmaValue(t, db, "journal_mode"); got != "wal" {
		t.Errorf("journal_mode = %s, want wal", got)
	}
	if got := pragmaValue(t, db, "busy_timeout"); got != "5000" {
		t.Errorf("busy_timeout = %s, want 5000", got)
	}
	if got := pragmaValue(t, db, "synchronous"); got != "1" {
		t.Errorf("synchronous = %s, want 1", got)
	}
}

func TestOpenDBWithOptionsHonorsBusyTimeout(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "aura.db")
	db, err := OpenDBWithOptions(ctx, dsn, OpenOptions{BusyTimeout: 1500 * time.Millisecond})
	if err != nil {
		t.Fatalf("OpenDBWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if got := pragmaValue(t, db, "busy_timeout"); got != "1500" {
		t.Errorf("busy_timeout = %s, want 1500 from options", got)
	}
	for _, p := range []struct{ name, want string }{{"foreign_keys", "1"}, {"journal_mode", "wal"}, {"synchronous", "1"}} {
		if got := pragmaValue(t, db, p.name); got != p.want {
			t.Errorf("%s = %s, want %s", p.name, got, p.want)
		}
	}
}

func TestOpenDBAppliesPolicyToEveryConnection(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "aura.db")
	db, err := OpenDB(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(0)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var got string
			if err := db.QueryRowContext(context.Background(), "PRAGMA foreign_keys").Scan(&got); err != nil {
				t.Errorf("pragma foreign_keys: %v", err)
				return
			}
			if got != "1" {
				t.Errorf("foreign_keys = %s, want 1", got)
			}
		}()
	}
	wg.Wait()
}

func TestOpenDBPathWithSpace(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "dir with space")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dsn := filepath.Join(dir, "aura.db")
	db, err := OpenDB(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	ro, err := openReadOnly(ctx, dsn)
	if err != nil {
		t.Fatalf("openReadOnly: %v", err)
	}
	t.Cleanup(func() { _ = ro.Close() })
	var count int
	if err := ro.QueryRowContext(ctx, `SELECT COUNT(*) FROM session`).Scan(&count); err != nil {
		t.Fatalf("query read-only db: %v", err)
	}
}

func TestEnsureOwnerOnlySecuresExistingDatabaseSidecars(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "aura.db")
	if err := os.WriteFile(dsn, []byte("database"), 0o644); err != nil {
		t.Fatalf("write database: %v", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.WriteFile(dsn+suffix, []byte("sidecar"), 0o644); err != nil {
			t.Fatalf("write %s: %v", suffix, err)
		}
	}

	if err := ensureOwnerOnly(dsn); err != nil {
		t.Fatalf("ensureOwnerOnly: %v", err)
	}
	for _, path := range []string{dsn, dsn + "-wal", dsn + "-shm"} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", filepath.Base(path), err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %o, want 600", filepath.Base(path), got)
		}
	}
}

func TestEnsureOwnerOnlyRejectsUnsafeDatabaseFiles(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("target"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(dir, "aura.db")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink database: %v", err)
	}
	if err := ensureOwnerOnly(link); err == nil {
		t.Fatal("ensureOwnerOnly accepted a database symlink")
	}

	nonRegular := filepath.Join(dir, "directory.db")
	if err := os.Mkdir(nonRegular, 0o700); err != nil {
		t.Fatalf("mkdir database path: %v", err)
	}
	if err := ensureOwnerOnly(nonRegular); err == nil {
		t.Fatal("ensureOwnerOnly accepted a non-regular database path")
	}

	sidecarDB := filepath.Join(dir, "sidecar.db")
	if err := os.WriteFile(sidecarDB, []byte("database"), 0o600); err != nil {
		t.Fatalf("write sidecar database: %v", err)
	}
	if err := os.Symlink(target, sidecarDB+"-wal"); err != nil {
		t.Fatalf("symlink sidecar: %v", err)
	}
	if err := ensureOwnerOnly(sidecarDB); err == nil {
		t.Fatal("ensureOwnerOnly accepted a sidecar symlink")
	}
}

func TestOpenReadOnlyRejectsUnsafePermissionsWithoutMutation(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "aura.db")
	db, err := OpenDB(context.Background(), dsn)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod directory: %v", err)
	}
	if err := os.Chmod(dsn, 0o644); err != nil {
		t.Fatalf("chmod database: %v", err)
	}

	if _, err := OpenReadOnly(context.Background(), dsn); err == nil {
		t.Fatal("OpenReadOnly accepted unsafe permissions")
	}
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		t.Fatalf("stat directory: %v", err)
	}
	dbInfo, err := os.Lstat(dsn)
	if err != nil {
		t.Fatalf("stat database: %v", err)
	}
	if dirInfo.Mode().Perm() != 0o755 || dbInfo.Mode().Perm() != 0o644 {
		t.Fatalf("OpenReadOnly mutated unsafe modes: directory=%o database=%o", dirInfo.Mode().Perm(), dbInfo.Mode().Perm())
	}
}

func TestOpenReadOnlyRejectsUnsafeSidecarWithoutMutation(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "aura.db")
	db, err := OpenDB(context.Background(), dsn)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}
	sidecar := dsn + "-wal"
	if err := os.WriteFile(sidecar, []byte("sidecar"), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	if _, err := OpenReadOnly(context.Background(), dsn); err == nil {
		t.Fatal("OpenReadOnly accepted unsafe sidecar permissions")
	}
	info, err := os.Lstat(sidecar)
	if err != nil {
		t.Fatalf("stat sidecar: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("OpenReadOnly mutated sidecar mode: %o", info.Mode().Perm())
	}
}

func TestArtifactPutMaxInt64Quota(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	artifacts := NewArtifactStore(db, t.TempDir(), math.MaxInt64)

	ref, err := artifacts.Put(ctx, bytes.NewReader([]byte("content")), &ArtifactMetadata{
		ID: "artifact-1", SessionID: "session-1", Filename: "f.bin", MediaType: "application/octet-stream",
	})
	if err != nil {
		t.Fatalf("Put with MaxInt64 quota: %v", err)
	}
	if ref.SizeBytes != int64(len("content")) {
		t.Errorf("SizeBytes = %d, want %d", ref.SizeBytes, len("content"))
	}
}

func TestMigrateAppliesFoundationalSchema(t *testing.T) {
	db := newTestDB(t)

	for _, table := range []string{"schema_migration", "session", "runtime_event", "blob", "artifact_ref", "ingress_dedupe"} {
		var name string
		err := db.QueryRowContext(context.Background(),
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing: %v", table, err)
		}
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}

func TestMigrateChecksumMismatch(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	if _, err := db.ExecContext(ctx, `UPDATE schema_migration SET checksum = 'tampered' WHERE version = 1`); err != nil {
		t.Fatalf("tamper checksum: %v", err)
	}

	err := Migrate(ctx, db)
	if err == nil {
		t.Fatal("expected checksum mismatch error, got nil")
	}
	code, ok := CodeOf(err)
	if !ok || code != ErrorCodeMigrationChecksumMismatch {
		t.Fatalf("CodeOf(err) = %v, %v; want %s", code, ok, ErrorCodeMigrationChecksumMismatch)
	}
}

func newSession(id string) Session {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return Session{
		ID:        id,
		OwnerID:   "owner-1",
		CreatedAt: now,
		UpdatedAt: now,
		Metadata:  json.RawMessage(`{"k":"v"}`),
	}
}

func TestSessionCreateGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionService(db)

	want := newSession("session-1")
	if err := sessions.Create(ctx, &want); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := sessions.Get(ctx, want.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != want.ID || got.OwnerID != want.OwnerID {
		t.Errorf("Get() = %+v, want %+v", got, want)
	}
	if string(got.Metadata) != string(want.Metadata) {
		t.Errorf("Metadata = %s, want %s", got.Metadata, want.Metadata)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, want.CreatedAt)
	}
}

func TestSessionGetMissing(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionService(db)

	if _, err := sessions.Get(ctx, "does-not-exist"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Get() error = %v, want wrapped sql.ErrNoRows", err)
	}
}

func newEvent(sessionID string, sequence uint64) RuntimeEvent {
	return RuntimeEvent{
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
	if err := NewSessionService(db).Create(context.Background(), &sess); err != nil {
		t.Fatalf("create session %s: %v", id, err)
	}
}

func TestEventAppendIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	events := NewEventStore(db)

	e := newEvent("session-1", 1)
	if err := events.Append(ctx, &e); err != nil {
		t.Fatalf("first Append: %v", err)
	}
	if err := events.Append(ctx, &e); err != nil {
		t.Fatalf("repeat Append (idempotent): %v", err)
	}

	last, err := events.LastSequence(ctx, "session-1")
	if err != nil {
		t.Fatalf("LastSequence: %v", err)
	}
	if last != 1 {
		t.Errorf("LastSequence = %d, want 1 (no duplicate row)", last)
	}
}

func TestEventAppendSequenceConflict(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	events := NewEventStore(db)

	first := newEvent("session-1", 1)
	if err := events.Append(ctx, &first); err != nil {
		t.Fatalf("first Append: %v", err)
	}

	second := newEvent("session-1", 1)
	second.ID = "a-different-event-id"
	err := events.Append(ctx, &second)
	if err == nil {
		t.Fatal("expected event_sequence_conflict, got nil")
	}
	code, ok := CodeOf(err)
	if !ok || code != ErrorCodeEventSequenceConflict {
		t.Fatalf("CodeOf(err) = %v, %v; want %s", code, ok, ErrorCodeEventSequenceConflict)
	}
}

func TestEventAppendBatchAtomicity(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	events := NewEventStore(db)

	conflicting := newEvent("session-1", 1)
	conflicting.ID = "conflicting-event"
	if err := events.Append(ctx, &conflicting); err != nil {
		t.Fatalf("seed conflicting event: %v", err)
	}

	batch := []RuntimeEvent{newEvent("session-1", 2), newEvent("session-1", 1)}
	if err := events.AppendBatch(ctx, batch); err == nil {
		t.Fatal("expected AppendBatch to fail on sequence conflict")
	}

	last, err := events.LastSequence(ctx, "session-1")
	if err != nil {
		t.Fatalf("LastSequence: %v", err)
	}
	if last != 1 {
		t.Errorf("LastSequence = %d, want 1 (batch must not partially apply)", last)
	}
}

func TestEventAppendConcurrentDistinctSequences(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	events := NewEventStore(db)

	const n = 50
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(seq uint64) {
			defer wg.Done()
			e := newEvent("session-1", seq)
			errs[seq-1] = events.Append(ctx, &e)
		}(uint64(i + 1))
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Append sequence %d: %v", i+1, err)
		}
	}

	sessions := NewSessionService(db)
	got, err := sessions.ListEvents(ctx, "session-1", 0, n+10)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(got) != n {
		t.Fatalf("ListEvents returned %d events, want %d", len(got), n)
	}
	for i, e := range got {
		if e.Sequence != uint64(i+1) {
			t.Fatalf("event[%d].Sequence = %d, want %d (no gaps or reordering)", i, e.Sequence, i+1)
		}
	}
}

func TestEventAppendConcurrentSequenceCollision(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	events := NewEventStore(db)

	a := newEvent("session-1", 1)
	a.ID = "event-a"
	b := newEvent("session-1", 1)
	b.ID = "event-b"

	var wg sync.WaitGroup
	results := make([]error, 2)
	for i, e := range []RuntimeEvent{a, b} {
		wg.Add(1)
		go func(idx int, ev RuntimeEvent) {
			defer wg.Done()
			results[idx] = events.Append(ctx, &ev)
		}(i, e)
	}
	wg.Wait()

	successes, conflicts := 0, 0
	for _, err := range results {
		switch err {
		case nil:
			successes++
		default:
			if code, ok := CodeOf(err); ok && code == ErrorCodeEventSequenceConflict {
				conflicts++
			} else {
				t.Fatalf("unexpected error: %v", err)
			}
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d, want 1 and 1", successes, conflicts)
	}
}
