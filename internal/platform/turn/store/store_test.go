package turnstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	"github.com/anggasct/aura/internal/platform/turn"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	return sqlitetest.OpenMigrated(t, Schema())
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

func seedPersonaSession(t *testing.T, db *sql.DB, id, ownerID, agentID string) {
	t.Helper()
	now := time.Now().UTC()
	sess := turn.Session{
		ID:        id,
		OwnerID:   ownerID,
		AgentID:   agentID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := NewSessionService(db).Create(t.Context(), &sess); err != nil {
		t.Fatalf("seed session %s: %v", id, err)
	}
}

func countRows(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("countRows(%s): %v", query, err)
	}
	return n
}

func wantCode(t *testing.T, err error, want ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with code %s, got nil", want)
	}
	got, ok := fault.CodeOf[ErrorCode](err)
	if !ok {
		t.Fatalf("error %v carries no store code, want %s", err, want)
	}
	if got != want {
		t.Errorf("error code = %s, want %s", got, want)
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
	if err := NewSessionService(db).Create(context.Background(), &sess); err != nil {
		t.Fatalf("create session %s: %v", id, err)
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

func TestSessionRecordsPersona(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionService(db)

	kuma := newSession("kuma/discord:home/channel:1")
	kuma.AgentID = "kuma"
	legacy := newSession("channel:1")
	for _, sess := range []*turn.Session{&kuma, &legacy} {
		if err := sessions.Create(ctx, sess); err != nil {
			t.Fatalf("Create %s: %v", sess.ID, err)
		}
	}
	for id, want := range map[string]string{kuma.ID: "kuma", legacy.ID: "main"} {
		got, err := sessions.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get %s: %v", id, err)
		}
		if got.AgentID != want {
			t.Errorf("session %s agent = %q, want %q", id, got.AgentID, want)
		}
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
	code, ok := fault.CodeOf[ErrorCode](err)
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

	batch := []turn.Event{newEvent("session-1", 2), newEvent("session-1", 1)}
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
	for i, e := range []turn.Event{a, b} {
		wg.Add(1)
		go func(idx int, ev turn.Event) {
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
			if code, ok := fault.CodeOf[ErrorCode](err); ok && code == ErrorCodeEventSequenceConflict {
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

func TestListSessionEventsFiltersByKindAndIDPrefix(t *testing.T) {
	ctx := t.Context()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	mustCreateSession(t, db, "session-2")
	events := NewEventStore(db)
	seed := []struct {
		session string
		id      string
		kind    string
	}{
		{"session-1", "handoff-reply-1", EventKindADK},
		{"session-1", "other-2", EventKindADK},
		{"session-1", "handoff-reply-3", "message"},
		{"session-1", "handoff-reply-4", EventKindADK},
		{"session-2", "handoff-reply-5", EventKindADK},
	}
	for i, s := range seed {
		ev := newEvent(s.session, 0)
		ev.ID = s.id
		ev.Kind = s.kind
		if _, err := events.AppendSequenced(ctx, s.session, &ev); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	ts := NewTurnStore(db)
	got, err := ts.ListSessionEvents(ctx, "session-1", turn.EventFilter{
		AfterSequence: 0,
		Kind:          EventKindADK,
		IDPrefix:      "handoff-reply-",
		Limit:         10,
	})
	if err != nil {
		t.Fatalf("ListSessionEvents: %v", err)
	}
	var ids []string
	for i := range got {
		ids = append(ids, got[i].ID)
	}
	if !slices.Equal(ids, []string{"handoff-reply-1", "handoff-reply-4"}) {
		t.Fatalf("ids = %v, want [handoff-reply-1 handoff-reply-4]", ids)
	}

	afterFirst, err := ts.ListSessionEvents(ctx, "session-1", turn.EventFilter{
		AfterSequence: got[0].Sequence,
		Kind:          EventKindADK,
		IDPrefix:      "handoff-reply-",
		Limit:         1,
	})
	if err != nil {
		t.Fatalf("ListSessionEvents after first: %v", err)
	}
	if len(afterFirst) != 1 || afterFirst[0].ID != "handoff-reply-4" {
		t.Fatalf("afterFirst = %+v, want handoff-reply-4", afterFirst)
	}

	if _, err := ts.ListSessionEvents(ctx, "session-1", turn.EventFilter{Limit: -1}); err == nil {
		t.Fatal("negative limit accepted")
	} else {
		wantCode(t, err, ErrorCodeInvalidArgument)
	}
}

func TestAcceptedTurnsReportsOnlyAcceptedPairs(t *testing.T) {
	ctx := t.Context()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-a")
	mustCreateSession(t, db, "session-b")
	events := NewEventStore(db)
	seed := []struct {
		session string
		turnID  string
		kind    string
	}{
		{"session-a", "turn-1", turn.EventKindTurnAccepted},
		{"session-b", "turn-2", turn.EventKindTurnAccepted},
		{"session-a", "turn-3", "turn.cancelled"},
	}
	for i, s := range seed {
		ev := newEvent(s.session, 0)
		ev.ID = fmt.Sprintf("ev-%d", i)
		ev.TurnID = s.turnID
		ev.Kind = s.kind
		if _, err := events.AppendSequenced(ctx, s.session, &ev); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	ts := NewTurnStore(db)
	empty, err := ts.AcceptedTurns(ctx, nil)
	if err != nil || empty != nil {
		t.Fatalf("AcceptedTurns(nil) = %v, %v; want nil, nil", empty, err)
	}

	got, err := ts.AcceptedTurns(ctx, []turn.SessionTurn{
		{SessionID: "session-a", TurnID: "turn-1"},
		{SessionID: "session-a", TurnID: "turn-2"},
		{SessionID: "session-a", TurnID: "turn-3"},
		{SessionID: "session-missing", TurnID: "turn-1"},
	})
	if err != nil {
		t.Fatalf("AcceptedTurns: %v", err)
	}
	want := []turn.SessionTurn{{SessionID: "session-a", TurnID: "turn-1"}}
	if !slices.Equal(got, want) {
		t.Fatalf("AcceptedTurns = %+v, want %+v", got, want)
	}
}
