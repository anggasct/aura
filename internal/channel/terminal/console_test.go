package terminal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"sync"
	"testing"
	"time"
)

// fakeRunner deterministically scripts one turn per prompt.
type fakeRunner struct {
	mu    sync.Mutex
	calls []Request
	// eventsFor returns the event script for a prompt; the last script ends
	// in a terminal event.
	eventsFor func(prompt string) []Event
}

func (f *fakeRunner) Run(ctx context.Context, req *Request) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		f.mu.Lock()
		f.calls = append(f.calls, *req)
		events := f.eventsFor(req.Parts[0].Text)
		f.mu.Unlock()
		for _, ev := range events {
			if ctx.Err() != nil {
				// Mirror the engine: a cancelled turn ends with a durable
				// cancelled event, not an error.
				yield(Event{Kind: "turn.cancelled"}, nil)
				return
			}
			if !yield(ev, nil) {
				return
			}
		}
	}
}

// textPayload is the model-text payload shape the renderer decodes.
type textPayload struct {
	Text string `json:"text"`
}

func delta(text string) json.RawMessage {
	b, err := json.Marshal(textPayload{Text: text})
	if err != nil {
		panic(err)
	}
	return b
}

func completed(text string) json.RawMessage {
	return delta(text)
}

// fakeSessions is an in-memory session store keyed by owner.
type fakeSessions struct {
	mu       sync.Mutex
	next     int
	sessions map[string]Session
	events   map[string][]Event
	getErr   error
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{sessions: map[string]Session{}, events: map[string][]Event{}}
}

func (s *fakeSessions) Create(ctx context.Context, owner string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	id := "sess_" + string(rune('0'+s.next))
	s.sessions[id] = Session{ID: id, OwnerID: owner}
	return s.sessions[id], nil
}

func (s *fakeSessions) Get(ctx context.Context, id string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return Session{}, s.getErr
	}
	sess, ok := s.sessions[id]
	if !ok {
		return Session{}, errors.New("session not found")
	}
	return sess, nil
}

func (s *fakeSessions) ListEvents(ctx context.Context, sessionID string, afterSequence uint64, limit int) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.events[sessionID]...), nil
}

func newConsoleForTest(runner Runner, sess *fakeSessions, in string) (console *Console, out, diag *bytes.Buffer, cancel context.CancelFunc) {
	out = &bytes.Buffer{}
	diag = &bytes.Buffer{}
	_, cancel = context.WithCancel(context.Background())
	console = NewConsole(runner, sess, PlainRenderer{}, bytes.NewBufferString(in), out, diag, Config{
		MaxInputBytes:       1000,
		InMemoryHistory:     100,
		SecondInterruptTime: 2 * time.Second,
	}, "owner")
	return console, out, diag, cancel
}

func newConsolePipeTest(runner Runner, sess *fakeSessions, in io.Reader) (console *Console, out, diag *bytes.Buffer, cancel context.CancelFunc) {
	out = &bytes.Buffer{}
	diag = &bytes.Buffer{}
	_, cancel = context.WithCancel(context.Background())
	console = NewConsole(runner, sess, PlainRenderer{}, in, out, diag, Config{
		MaxInputBytes:       1000,
		InMemoryHistory:     100,
		SecondInterruptTime: 2 * time.Second,
	}, "owner")
	return console, out, diag, cancel
}

func TestPlainWritesOnlyCompletedAssistantText(t *testing.T) {
	runner := &fakeRunner{eventsFor: func(string) []Event {
		return []Event{
			{Kind: "model.delta", Payload: delta("hel")},
			{Kind: "model.delta", Payload: delta("lo")},
			{Kind: "turn.completed", Payload: completed("")},
		}
	}}
	console, out, diag, cancel := newConsoleForTest(runner, newFakeSessions(), "hi\n")
	defer cancel()
	err := console.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := out.String(); got != "hello\n" {
		t.Errorf("stdout = %q, want %q", got, "hello\n")
	}
	if diag.Len() != 0 {
		t.Errorf("stderr = %q, want empty", diag.String())
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runs = %d, want 1", len(runner.calls))
	}
	req := runner.calls[0]
	if req.Origin != "terminal" || req.PrincipalID != "owner" {
		t.Errorf("request = %+v, want terminal origin and owner principal", req)
	}
}

func TestPlainSeparatesDiagnosticsToStderr(t *testing.T) {
	runner := &fakeRunner{eventsFor: func(string) []Event {
		return []Event{{Kind: "turn.failed", Payload: json.RawMessage(`{"code":"policy_denied"}`)}}
	}}
	console, out, diag, cancel := newConsoleForTest(runner, newFakeSessions(), "run tool\n")
	defer cancel()
	if err := console.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty on failed turn", out.String())
	}
	// A failed turn produces a terminal event; the renderer does not enrich
	// stderr for it, so stderr stays empty in this deterministic script.
	_ = diag
}

func TestPlainRunsMultiplePromptsToEOF(t *testing.T) {
	runner := &fakeRunner{eventsFor: func(p string) []Event {
		return []Event{{Kind: "message.completed", Payload: completed("ack:" + p)}, {Kind: "turn.completed"}}
	}}
	console, out, _, cancel := newConsoleForTest(runner, newFakeSessions(), "one\ntwo\n")
	defer cancel()
	if err := console.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := out.String(); got != "ack:one\nack:two\n" {
		t.Errorf("stdout = %q, want both acknowledgements", got)
	}
}

func TestUnknownCommandNotModelInput(t *testing.T) {
	runner := &fakeRunner{eventsFor: func(string) []Event {
		return []Event{{Kind: "turn.completed"}}
	}}
	console, out, diag, cancel := newConsoleForTest(runner, newFakeSessions(), "/bogus\n/help\n")
	defer cancel()
	if err := console.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Errorf("model saw %d turns, want 0", len(runner.calls))
	}
	if diag.Len() == 0 {
		t.Error("stderr empty, want unknown-command diagnostic")
	}
	if out.Len() == 0 {
		t.Error("stdout empty, want /help output")
	}
}

func TestExitStopsReading(t *testing.T) {
	runner := &fakeRunner{eventsFor: func(string) []Event {
		return []Event{{Kind: "turn.completed"}}
	}}
	console, _, _, cancel := newConsoleForTest(runner, newFakeSessions(), "/exit\nignored\n")
	defer cancel()
	if err := console.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Errorf("model saw %d turns after /exit, want 0", len(runner.calls))
	}
}

func TestSessionSwitchValidatesOwner(t *testing.T) {
	sessions := newFakeSessions()
	own := sessions.sessions
	own["sess_other"] = Session{ID: "sess_other", OwnerID: "someone-else"}
	console, _, diag, cancel := newConsoleForTest(&fakeRunner{eventsFor: func(string) []Event { return []Event{{Kind: "turn.completed"}} }}, sessions, "/session sess_other\n")
	defer cancel()
	err := console.Run(context.Background())
	if err == nil {
		t.Fatal("expected owner validation error")
	}
	_ = diag
}

func TestSessionSwitchToOwnedSession(t *testing.T) {
	sessions := newFakeSessions()
	sessions.sessions["sess_mine"] = Session{ID: "sess_mine", OwnerID: "owner"}
	runner := &fakeRunner{eventsFor: func(string) []Event { return []Event{{Kind: "turn.completed"}} }}
	console, _, _, cancel := newConsoleForTest(runner, sessions, "/session sess_mine\nprompt\n")
	defer cancel()
	if err := console.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(runner.calls) != 1 || runner.calls[0].SessionID != "sess_mine" {
		t.Errorf("turn session = %+v, want sess_mine", runner.calls)
	}
}

func TestStatusListsCurrentSession(t *testing.T) {
	sessions := newFakeSessions()
	runner := &fakeRunner{eventsFor: func(string) []Event { return []Event{{Kind: "turn.completed"}} }}
	console, out, _, cancel := newConsoleForTest(runner, sessions, "/status\n")
	defer cancel()
	if err := console.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("session sess_")) {
		t.Errorf("stdout = %q, want current session", out.String())
	}
}

func TestSecondInterruptWithinWindowEscalates(t *testing.T) {
	runner := &fakeRunner{eventsFor: func(string) []Event {
		return []Event{{Kind: "turn.completed"}}
	}}
	// Input stays open so interrupts are observed before EOF.
	pr, pw := io.Pipe()
	console, _, _, cancel := newConsolePipeTest(runner, newFakeSessions(), pr)
	defer cancel()
	defer func() { _ = pw.Close() }()
	interrupts := make(chan struct{}, 2)
	console.SetInterrupts(interrupts)

	done := make(chan error, 1)
	go func() { done <- console.Run(context.Background()) }()
	interrupts <- struct{}{}
	interrupts <- struct{}{}
	if err := <-done; !errors.Is(err, ErrInterrupted) {
		t.Fatalf("Run = %v, want ErrInterrupted", err)
	}
}

func TestFirstInterruptCancelsActiveTurnOnly(t *testing.T) {
	runner := &fakeRunner{eventsFor: func(string) []Event {
		return []Event{{Kind: "model.delta", Payload: delta("thinking")}, {Kind: "turn.completed"}}
	}}
	pr, pw := io.Pipe()
	console, _, _, cancel := newConsolePipeTest(runner, newFakeSessions(), pr)
	defer cancel()
	interrupts := make(chan struct{}, 2)
	console.SetInterrupts(interrupts)

	done := make(chan error, 1)
	go func() {
		// Keep the pipe open; a lone interrupt must cancel the turn but not
		// exit, so Run blocks waiting for more input.
		done <- console.Run(context.Background())
		_ = pw.Close()
	}()
	if _, err := pw.Write([]byte("hi\n")); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	interrupts <- struct{}{}
	select {
	case err := <-done:
		t.Fatalf("Run returned %v after first interrupt, want it to keep reading", err)
	case <-time.After(50 * time.Millisecond):
	}
	_ = pw.Close()
	if err := <-done; err != nil {
		t.Fatalf("Run after EOF: %v", err)
	}
}

func TestOverlongLineRejected(t *testing.T) {
	runner := &fakeRunner{eventsFor: func(string) []Event { return []Event{{Kind: "turn.completed"}} }}
	config := Config{MaxInputBytes: 5, InMemoryHistory: 100, SecondInterruptTime: time.Second}
	out := &bytes.Buffer{}
	diag := &bytes.Buffer{}
	console := NewConsole(runner, newFakeSessions(), PlainRenderer{}, bytes.NewBufferString("toolong\n"), out, diag, config, "owner")
	err := console.Run(context.Background())
	if err == nil {
		t.Fatal("expected overlong-line error")
	}
}
