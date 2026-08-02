package runtime

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/store"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func newSessionTestDB(t *testing.T) (*sql.DB, store.SessionService, store.EventStore) {
	t.Helper()
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "aura.db")
	db, err := store.OpenDB(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db, store.NewSessionService(db), store.NewEventStore(db)
}

func TestADKSessionServiceCreateGet(t *testing.T) {
	_, sessions, events := newSessionTestDB(t)
	svc, err := NewADKSessionService(sessions, events)
	if err != nil {
		t.Fatalf("NewADKSessionService: %v", err)
	}

	resp, err := svc.Create(context.Background(), &session.CreateRequest{
		AppName:   "aura",
		UserID:    "user-1",
		SessionID: "session-1",
		State:     map[string]any{"mode": "fast"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if resp.Session.ID() != "session-1" {
		t.Errorf("Session.ID = %q, want session-1", resp.Session.ID())
	}
	if resp.Session.UserID() != "user-1" {
		t.Errorf("Session.UserID = %q, want user-1", resp.Session.UserID())
	}
	if v, err := resp.Session.State().Get("mode"); err != nil || v != "fast" {
		t.Errorf("state mode = %v, %v; want fast, nil", v, err)
	}

	got, err := svc.Get(context.Background(), &session.GetRequest{SessionID: "session-1"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Session.ID() != "session-1" || got.Session.UserID() != "user-1" {
		t.Errorf("Get = %+v", got.Session)
	}
	if got.Session.Events().Len() != 0 {
		t.Errorf("events = %d, want 0", got.Session.Events().Len())
	}
}

func TestADKSessionServiceAppendAndReload(t *testing.T) {
	_, sessions, events := newSessionTestDB(t)
	svc, err := NewADKSessionService(sessions, events)
	if err != nil {
		t.Fatalf("NewADKSessionService: %v", err)
	}
	ctx := context.Background()

	created, err := svc.Create(ctx, &session.CreateRequest{AppName: "aura", UserID: "user-1", SessionID: "session-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	ev := &session.Event{
		ID:           "event-1",
		Timestamp:    time.Now().UTC().Truncate(time.Millisecond),
		InvocationID: "inv-1",
		Branch:       "agent_1",
		Author:       "model",
	}
	ev.Content = genai.NewContentFromText("hello", genai.RoleModel)
	if err := svc.AppendEvent(ctx, created.Session, ev); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	got, err := svc.Get(ctx, &session.GetRequest{SessionID: "session-1"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Session.Events().Len() != 1 {
		t.Fatalf("events = %d, want 1", got.Session.Events().Len())
	}
	reloaded := got.Session.Events().At(0)
	if reloaded.ID != "event-1" || reloaded.InvocationID != "inv-1" || reloaded.Branch != "agent_1" {
		t.Errorf("reloaded event = %+v, want preserved fidelity", reloaded)
	}
	if !reflect.DeepEqual(reloaded.Content, ev.Content) {
		t.Errorf("content = %+v, want %+v", reloaded.Content, ev.Content)
	}
}

func TestADKSessionServiceUnsupportedOps(t *testing.T) {
	_, sessions, events := newSessionTestDB(t)
	svc, err := NewADKSessionService(sessions, events)
	if err != nil {
		t.Fatalf("NewADKSessionService: %v", err)
	}
	ctx := context.Background()

	if _, err := svc.List(ctx, &session.ListRequest{}); err == nil {
		t.Fatal("List returned nil, want not-supported error")
	}
	if err := svc.Delete(ctx, &session.DeleteRequest{}); err == nil {
		t.Fatal("Delete returned nil, want not-supported error")
	}
	if _, err := svc.Get(ctx, &session.GetRequest{SessionID: "missing"}); err == nil {
		t.Fatal("Get(missing) returned nil, want error")
	}
	if _, err := svc.Create(ctx, &session.CreateRequest{UserID: ""}); err == nil {
		t.Fatal("Create with empty user accepted")
	}
}
