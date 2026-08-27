//go:build e2e

package terminal_test

import (
	"bytes"
	"context"
	"encoding/json"
	"iter"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/channel/terminal"
	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/store"
)

// e2eRunner is the production adapter shape: it maps the console request onto
// the runtime turn boundary and surfaces durable events back to the console.
type e2eRunner struct {
	engine runtime.AgentRuntime
}

func (r e2eRunner) Run(ctx context.Context, req *terminal.Request) iter.Seq2[terminal.Event, error] {
	return func(yield func(terminal.Event, error) bool) {
		parts := make([]runtime.InputPart, len(req.Parts))
		for i := range req.Parts {
			parts[i] = runtime.InputPart{Text: req.Parts[i].Text}
		}
		runtimeReq := &runtime.TurnRequest{
			SessionID:   req.SessionID,
			PrincipalID: req.PrincipalID,
			Origin:      runtime.OriginTerminal,
			Parts:       parts,
		}
		for ev, err := range r.engine.Run(ctx, runtimeReq) {
			if err != nil {
				yield(terminal.Event{}, err)
				return
			}
			if !yield(terminal.Event{
				Kind:    ev.Kind,
				Author:  ev.Author,
				TurnID:  ev.TurnID,
				Payload: json.RawMessage(ev.Payload),
			}, nil) {
				return
			}
		}
	}
}

// e2eSessions adapts the store session service onto the console port.
type e2eSessions struct {
	sessions store.SessionService
}

func (s e2eSessions) Create(ctx context.Context, owner string) (terminal.Session, error) {
	internal := &store.Session{ID: "e2e-session", OwnerID: owner, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := s.sessions.Create(ctx, internal); err != nil {
		return terminal.Session{}, err
	}
	return terminal.Session{ID: internal.ID, OwnerID: internal.OwnerID}, nil
}

func (s e2eSessions) Get(ctx context.Context, id string) (terminal.Session, error) {
	internal, err := s.sessions.Get(ctx, id)
	return terminal.Session{ID: internal.ID, OwnerID: internal.OwnerID}, err
}

func (s e2eSessions) ListEvents(ctx context.Context, sessionID string, after uint64, limit int) ([]terminal.Event, error) {
	events, err := s.sessions.ListEvents(ctx, sessionID, after, limit)
	if err != nil {
		return nil, err
	}
	out := make([]terminal.Event, len(events))
	for i := range events {
		out[i] = terminal.Event{Kind: events[i].Kind, Author: events[i].Author}
	}
	return out, nil
}

// TestTerminalVerticalSlice drives the plain console over a real engine and a
// real SQLite store, with the deterministic fake executor standing in for the
// model. This is the first end-to-end slice: durable session, submitted turn,
// event stream, completed text on stdout, clean EOF.
func TestTerminalVerticalSlice(t *testing.T) {
	db, err := store.OpenDB(context.Background(), filepath.Join(t.TempDir(), "aura.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	sessions := store.NewSessionService(db)
	events := store.NewEventStore(db)
	executor := runtime.NewFakeExecutor([]runtime.FakeStep{
		{Kind: runtime.EventKindModelDelta, Payload: json.RawMessage(`{"content":{"parts":[{"text":"hello from the slice"}]}}`)},
	})
	engine, err := runtime.NewEngine(runtime.Config{}, events, store.NewDedupeStore(db), executor, nil)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}

	out := &bytes.Buffer{}
	diag := &bytes.Buffer{}
	console := terminal.NewConsole(
		e2eRunner{engine: engine},
		e2eSessions{sessions: sessions},
		terminal.PlainRenderer{},
		bytes.NewBufferString("hi\n"),
		out, diag,
		terminal.Config{MaxInputBytes: 1024, InMemoryHistory: 100, SecondInterruptTime: time.Second},
		"e2e-owner",
	)
	if err := console.Run(context.Background()); err != nil {
		t.Fatalf("console run: %v", err)
	}
	if got := out.String(); got != "hello from the slice\n" {
		t.Errorf("stdout = %q, want completed assistant text", got)
	}
	sess, err := sessions.Get(context.Background(), "e2e-session")
	if err != nil {
		t.Fatalf("durable session: %v", err)
	}
	if sess.OwnerID != "e2e-owner" {
		t.Errorf("session owner = %q, want e2e-owner", sess.OwnerID)
	}
	turns, err := sessions.ListEvents(context.Background(), "e2e-session", 0, 100)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(turns) == 0 {
		t.Fatal("no durable events recorded for the turn")
	}
}

// TestTerminalReplayCompletedEventWins proves the replay half of the render
// contract: events read back from durable storage drive the interactive
// renderer to the same authoritative completed message the live stream
// produced, and streamed partials never survive replay.
func TestTerminalReplayCompletedEventWins(t *testing.T) {
	db, err := store.OpenDB(context.Background(), filepath.Join(t.TempDir(), "aura.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	sessions := store.NewSessionService(db)
	events := store.NewEventStore(db)
	executor := runtime.NewFakeExecutor([]runtime.FakeStep{
		{Kind: runtime.EventKindModelDelta, Payload: json.RawMessage(`{"content":{"parts":[{"text":"partial "}]}}`)},
		{Kind: runtime.EventKindModelDelta, Payload: json.RawMessage(`{"content":{"parts":[{"text":"stream"}]}}`)},
		{Kind: runtime.EventKindMessageCompleted, Payload: json.RawMessage(`{"content":{"parts":[{"text":"durable answer"}]}}`)},
	})
	engine, err := runtime.NewEngine(runtime.Config{}, events, store.NewDedupeStore(db), executor, nil)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	if err := sessions.Create(context.Background(), &store.Session{ID: "e2e-replay", OwnerID: "e2e-owner", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	runner := e2eRunner{engine: engine}
	req := &terminal.Request{SessionID: "e2e-replay", PrincipalID: "e2e-owner", Origin: "terminal", Parts: []terminal.Input{{Text: "hi"}}}
	for _, err := range runner.Run(context.Background(), req) {
		if err != nil {
			t.Fatalf("live turn: %v", err)
		}
	}

	stored, err := sessions.ListEvents(context.Background(), "e2e-replay", 0, 1000)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var replayed []terminal.Event
	for i := range stored {
		replayed = append(replayed, terminal.Event{Kind: stored[i].Kind, Author: stored[i].Author, Payload: stored[i].Payload})
	}
	out := &bytes.Buffer{}
	renderer := terminal.NewTTYRenderer(terminal.TTYOptions{Out: out, Width: func() int { return 80 }, Hz: 500})
	renderer.Begin()
	for _, ev := range replayed {
		renderer.Observe(ev)
	}
	if err := renderer.Finalize(false); err != nil {
		t.Fatalf("finalize replay: %v", err)
	}
	if !strings.Contains(out.String(), "durable answer") {
		t.Errorf("replayed output = %q, want the durable completed message", out.String())
	}
	if strings.Contains(out.String(), "partial stream") {
		t.Errorf("replayed output = %q, streamed partial must not win on replay", out.String())
	}
}
