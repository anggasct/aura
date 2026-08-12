package effect

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/store"
)

func newTestJournal(t *testing.T) (*Journal, *sql.DB) {
	t.Helper()
	db, err := store.OpenDB(context.Background(), t.TempDir()+"/effect.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	j, err := NewJournal(db, Options{
		Now: func() time.Time { return time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("new journal: %v", err)
	}
	if err := seedSession(t, db); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return j, db
}

func seedSession(t *testing.T, db *sql.DB) error {
	t.Helper()
	svc := store.NewSessionService(db)
	return svc.Create(context.Background(), &store.Session{
		ID:        "sess-1",
		OwnerID:   "owner-1",
		CreatedAt: time.Date(2026, 8, 12, 8, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 8, 12, 8, 0, 0, 0, time.UTC),
	})
}

func validPrepare(n int) *PrepareRequest {
	return &PrepareRequest{
		SessionID:       "sess-1",
		TurnID:          "turn-1",
		ToolCallID:      fmt.Sprintf("call-%d", n),
		IdempotencyKey:  fmt.Sprintf("idem-%d", n),
		Provider:        "telegram",
		Operation:       "send_message",
		Classification:  ClassificationEffectful,
		Request:         json.RawMessage(`{"chat":"@a","text":"hi"}`),
		EventSequence:   uint64(n),
		EventInvocation: "inv-1",
		EventBranch:     "main",
		EventAuthor:     "agent",
	}
}

func mustPrepare(t *testing.T, j *Journal, req *PrepareRequest) *Intent {
	t.Helper()
	intent, err := j.Prepare(context.Background(), req)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	return intent
}

func mustStart(t *testing.T, j *Journal, id string) *Intent {
	t.Helper()
	intent, err := j.Start(context.Background(), id)
	if err != nil {
		t.Fatalf("start %s: %v", id, err)
	}
	return intent
}

func intentCount(t *testing.T, db *sql.DB, where string, args ...any) int {
	t.Helper()
	var n int
	query := "SELECT COUNT(*) FROM effect_intent"
	if where != "" {
		query += " WHERE " + where
	}
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count intents (%s): %v", where, err)
	}
	return n
}

func eventCount(t *testing.T, db *sql.DB, kind string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM runtime_event WHERE kind = ?`, kind).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

func assertCode(t *testing.T, err error, want ErrorCode) {
	t.Helper()
	got, ok := CodeOf(err)
	if !ok {
		t.Fatalf("expected coded error %s, got unwrapped: %v", want, err)
	}
	if got != want {
		t.Fatalf("expected error code %s, got %s (err=%v)", want, got, err)
	}
}
