package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
		switch {
		case err == nil:
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
