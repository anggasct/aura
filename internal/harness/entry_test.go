package harness

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/runtime/engine"
	"github.com/anggasct/aura/internal/runtime/ingress"
	"github.com/anggasct/aura/internal/store"
)

func TestComposedRuntimeEntryPersistsTerminalBeforeReturn(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenDB(ctx, filepath.Join(t.TempDir(), "aura.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := store.NewSessionService(db).Create(ctx, &store.Session{ID: "entry-session", OwnerID: "owner"}); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	events := store.NewEventStore(db)
	engine, err := runtimeengine.NewEngine(runtimeengine.Config{}, events, store.NewDedupeStore(db), runtime.NewFakeExecutor([]runtime.FakeStep{{Kind: runtime.EventKindMessageCompleted, Payload: json.RawMessage(`{}`)}}), nil)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	var streamed []store.RuntimeEvent
	for event, streamErr := range engine.Run(ctx, &runtime.TurnRequest{
		TurnID:      "entry-turn",
		SessionID:   "entry-session",
		PrincipalID: "owner",
		Origin:      runtime.OriginInternal,
		Parts:       []runtimeingress.InputPart{{Text: "entry"}},
	}) {
		if streamErr != nil {
			t.Fatalf("Run: %v", streamErr)
		}
		streamed = append(streamed, event)
	}
	stored, err := store.NewSessionService(db).ListEvents(ctx, "entry-session", 0, 100)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(stored) != len(streamed) || len(stored) == 0 {
		t.Fatalf("stored events = %d, streamed = %d", len(stored), len(streamed))
	}
	for i := range stored {
		if stored[i].Sequence != uint64(i+1) || stored[i].ID != streamed[i].ID {
			t.Fatalf("event %d = %+v, stream = %+v", i, stored[i], streamed[i])
		}
	}
	last := stored[len(stored)-1]
	if last.Kind != runtime.EventKindTurnCompleted {
		t.Fatalf("last event kind = %q, want turn.completed", last.Kind)
	}
}
