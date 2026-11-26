package turnstore

import (
	"context"
	"encoding/json"
	"math"
	"testing"

	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/turn"
)

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

func TestSessionCreateDuplicateReturnsConflict(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")

	wantCode(t, NewSessionService(db).Create(ctx, &turn.Session{ID: "session-1"}), ErrorCodeSessionIDConflict)
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

func TestWorkflowSessionPrefixIsReserved(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	wantCode(t, NewSessionService(db).Create(ctx, &turn.Session{ID: "wf-x", OwnerID: "discord:1"}), ErrorCodeInvalidArgument)
	if got := countRows(t, db, `SELECT COUNT(*) FROM session WHERE id = 'wf-x'`); got != 0 {
		t.Fatalf("a refused reserved id left %d session rows", got)
	}
	ts := NewTurnStore(db)
	if err := sqlite.RunInTx(ctx, db, func(ctx context.Context) error {
		if err := ts.CreateSession(ctx, &turn.Session{ID: "wf-x", OwnerID: "discord:1"}); err != nil {
			t.Fatalf("CreateSession of a reserved id: %v", err)
		}
		wantCode(t, ts.CreateSession(ctx, &turn.Session{ID: "wf-x", OwnerID: "discord:1"}), ErrorCodeSessionIDConflict)
		return nil
	}); err != nil {
		t.Fatalf("RunInTx: %v", err)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM session WHERE id = 'wf-x' AND agent_id = 'main'`); got != 1 {
		t.Fatalf("committed run session rows = %d, want 1 with the default agent", got)
	}
}
