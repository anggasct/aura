package runtimeengine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"iter"
	"path/filepath"
	"testing"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/store/storetest"
)

var errEventWriteRefused = errors.New("event write refused")

type refusingEventStore struct {
	EventStore
	kind string
}

func (s *refusingEventStore) Append(ctx context.Context, ev *store.RuntimeEvent) error {
	if ev.Kind == s.kind {
		return errEventWriteRefused
	}
	return s.EventStore.Append(ctx, ev)
}

func (s *refusingEventStore) AppendSequenced(ctx context.Context, sessionID string, ev *store.RuntimeEvent) (uint64, error) {
	if ev.Kind == s.kind {
		return 0, errEventWriteRefused
	}
	return s.EventStore.AppendSequenced(ctx, sessionID, ev)
}

func (s *refusingEventStore) UpsertEvent(ctx context.Context, ev *store.RuntimeEvent) (sequence uint64, inserted bool, err error) {
	if ev.Kind == s.kind {
		return 0, false, errEventWriteRefused
	}
	return s.EventStore.UpsertEvent(ctx, ev)
}

type unsequencedExecutor struct {
	inner TurnExecutor
}

func (x unsequencedExecutor) Execute(ctx context.Context, req *turn.TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
	return func(yield func(store.RuntimeEvent, error) bool) {
		for ev, err := range x.inner.Execute(ctx, req) {
			ev.Sequence = 0
			if !yield(ev, err) {
				return
			}
		}
	}
}

func newRefusingTestRuntime(t *testing.T, executor TurnExecutor, refusedKind string, live bool) (*Engine, *sql.DB) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "aura.db")
	storetest.Seed(t, dsn)
	db, err := store.OpenDB(t.Context(), dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	events := store.NewEventStore(db)
	dedupe := store.NewDedupeStore(db)
	engine, err := NewEngine(&Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, &refusingEventStore{EventStore: events, kind: refusedKind}, dedupe, executor, nil)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	if live {
		backend := durable.NewFake()
		engine.sessionStore = newStubSessionStore(events, dedupe)
		engine.durableRuntime = backend
		backend.RegisterHandler("turn", engine.serveTurn)
	}
	return engine, db
}

func TestFailedEventWriteIsNeverStreamed(t *testing.T) {
	for _, tc := range []struct {
		name string
		live bool
	}{
		{name: "without durable config"},
		{name: "durable live", live: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor := unsequencedExecutor{inner: turn.NewFakeExecutor([]turn.FakeStep{{Kind: turn.EventKindMessageCompleted, Payload: json.RawMessage(`{}`)}})}
			engine, db := newRefusingTestRuntime(t, executor, turn.EventKindMessageCompleted, tc.live)
			mustCreateSession(t, db, "session-refused")

			streamed, err := collect(t, engine, sampleRequest("session-refused", "turn-refused"))
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			stored, err := store.NewDedupeStore(db).ListTurnEvents(context.Background(), "turn-refused")
			if err != nil {
				t.Fatalf("list turn events: %v", err)
			}

			for _, events := range [][]store.RuntimeEvent{streamed, stored} {
				for _, ev := range events {
					if ev.Kind == turn.EventKindMessageCompleted {
						t.Fatalf("event %s with a refused write reached the turn: %+v", ev.Kind, ev)
					}
				}
			}
			if len(streamed) == 0 || len(stored) == 0 {
				t.Fatalf("streamed = %d, stored = %d; want a terminal event in both", len(streamed), len(stored))
			}
			last := streamed[len(streamed)-1]
			var failure struct {
				Code turn.ErrorCode `json:"code"`
			}
			if err := json.Unmarshal(last.Payload, &failure); err != nil {
				t.Fatalf("decode terminal payload %s: %v", last.Payload, err)
			}
			if last.Kind != turn.EventKindTurnFailed || failure.Code != turn.ErrorCodeStorageUnavailable {
				t.Fatalf("last streamed = %s %s, want turn.failed with storage_unavailable", last.Kind, last.Payload)
			}
			if stored[len(stored)-1].Kind != turn.EventKindTurnFailed {
				t.Fatalf("last stored kind = %s, want turn.failed", stored[len(stored)-1].Kind)
			}
		})
	}
}

func TestRunStreamMatchesTheStoredTurn(t *testing.T) {
	executor := turn.NewFakeExecutor([]turn.FakeStep{{Kind: turn.EventKindMessageCompleted, Payload: json.RawMessage(`{}`)}})
	engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
	mustCreateSession(t, db, "session-entry")

	streamed, err := collect(t, engine, sampleRequest("session-entry", "turn-entry"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	stored, err := store.NewSessionService(db).ListEvents(context.Background(), "session-entry", 0, 100)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}

	if len(stored) == 0 || len(stored) != len(streamed) {
		t.Fatalf("stored events = %d, streamed = %d", len(stored), len(streamed))
	}
	for i := range stored {
		if stored[i].Sequence != uint64(i+1) || stored[i].ID != streamed[i].ID {
			t.Fatalf("event %d stored %+v, streamed %+v", i, stored[i], streamed[i])
		}
	}
	if last := stored[len(stored)-1]; last.Kind != turn.EventKindTurnCompleted {
		t.Fatalf("last stored kind = %q, want turn.completed", last.Kind)
	}
}
