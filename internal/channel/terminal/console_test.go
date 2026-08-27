package terminal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"strings"
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

type cancellationWithoutTerminalRunner struct {
	started chan struct{}
}

func (r *cancellationWithoutTerminalRunner) Run(ctx context.Context, _ *Request) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		close(r.started)
		<-ctx.Done()
	}
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
		return []Event{
			{Kind: "model.delta", Payload: delta("partial")},
			{Kind: "turn.failed", Payload: json.RawMessage(`{"code":"policy_denied"}`)},
		}
	}}
	console, out, diag, cancel := newConsoleForTest(runner, newFakeSessions(), "run tool\n")
	defer cancel()
	if err := console.Run(context.Background()); err == nil {
		t.Fatal("Run returned nil for failed turn")
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty on failed turn", out.String())
	}
	if got := diag.String(); got != "turn failed\n" {
		t.Errorf("stderr = %q, want failed-turn diagnostic", got)
	}
}

func TestPlainSuppressesCancelledPartialOutput(t *testing.T) {
	runner := &fakeRunner{eventsFor: func(string) []Event {
		return []Event{
			{Kind: "model.delta", Payload: delta("partial")},
			{Kind: "turn.cancelled"},
		}
	}}
	console, out, diag, cancel := newConsoleForTest(runner, newFakeSessions(), "cancel\n")
	defer cancel()
	if err := console.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty on cancelled turn", out.String())
	}
	if got := diag.String(); got != "turn cancelled\n" {
		t.Errorf("stderr = %q, want cancelled-turn diagnostic", got)
	}
}

func TestCancellationWithoutTerminalEventFails(t *testing.T) {
	runner := &cancellationWithoutTerminalRunner{started: make(chan struct{})}
	console, out, _, cleanup := newConsoleForTest(runner, newFakeSessions(), "unused\n")
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- console.runTurn(ctx, "prompt") }()
	<-runner.started
	cancel()
	if err := <-done; err == nil {
		t.Fatal("runTurn returned nil without a terminal event")
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty", out.String())
	}
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

func TestConfiguredSessionIsUsed(t *testing.T) {
	sessions := newFakeSessions()
	sessions.sessions["sess_resume"] = Session{ID: "sess_resume", OwnerID: "owner"}
	runner := &fakeRunner{eventsFor: func(string) []Event { return []Event{{Kind: "turn.completed"}} }}
	console, _, _, cancel := newConsoleForTest(runner, sessions, "prompt\n")
	defer cancel()
	console.SetSessionID("sess_resume")
	if err := console.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(runner.calls) != 1 || runner.calls[0].SessionID != "sess_resume" {
		t.Errorf("turn session = %+v, want sess_resume", runner.calls)
	}
}

func TestSessionSwitchRejectsOwnerlessSession(t *testing.T) {
	sessions := newFakeSessions()
	sessions.sessions["sess_ownerless"] = Session{ID: "sess_ownerless"}
	console, _, _, cancel := newConsoleForTest(&fakeRunner{eventsFor: func(string) []Event {
		return []Event{{Kind: "turn.completed"}}
	}}, sessions, "/session sess_ownerless\n")
	defer cancel()
	if err := console.Run(context.Background()); err == nil {
		t.Fatal("expected owner validation error")
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

func TestClosedInterruptSourceStopsWatcher(t *testing.T) {
	interrupts := make(chan struct{})
	close(interrupts)
	c := &Console{
		interrupts: interrupts,
		escalated:  make(chan struct{}, 1),
		now:        time.Now,
		config:     Config{SecondInterruptTime: time.Hour},
	}
	done := make(chan struct{})
	go func() {
		c.watchInterrupts(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watcher did not stop after interrupt source closed")
	}
	select {
	case <-c.escalated:
		t.Fatal("closed interrupt source escalated")
	default:
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

func TestOverlongUnterminatedLineRejected(t *testing.T) {
	runner := &fakeRunner{eventsFor: func(string) []Event { return []Event{{Kind: "turn.completed"}} }}
	config := Config{MaxInputBytes: 5, InMemoryHistory: 100, SecondInterruptTime: time.Second}
	console := NewConsole(runner, newFakeSessions(), PlainRenderer{}, bytes.NewBufferString("123456"), &bytes.Buffer{}, &bytes.Buffer{}, config, "owner")
	if err := console.Run(context.Background()); err == nil {
		t.Fatal("expected overlong-line error")
	}
}

func TestCancellationStopsCloseableReader(t *testing.T) {
	pr, pw := io.Pipe()
	console, _, _, _ := newConsolePipeTest(&fakeRunner{eventsFor: func(string) []Event {
		return []Event{{Kind: "turn.completed"}}
	}}, newFakeSessions(), pr)
	console.SetInputCloser(func() { _ = pr.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- console.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
	_ = pw.Close()
}

type closeTrackingReader struct {
	*bytes.Reader
	closed bool
}

func (r *closeTrackingReader) Close() error {
	r.closed = true
	return nil
}

func TestRunDoesNotCloseCallerReader(t *testing.T) {
	reader := &closeTrackingReader{Reader: bytes.NewReader(nil)}
	console := NewConsole(&fakeRunner{eventsFor: func(string) []Event {
		return []Event{{Kind: "turn.completed"}}
	}}, newFakeSessions(), PlainRenderer{}, reader, &bytes.Buffer{}, &bytes.Buffer{}, Config{
		MaxInputBytes:       100,
		SecondInterruptTime: time.Second,
	}, "owner")
	if err := console.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reader.closed {
		t.Fatal("Run closed the caller-owned reader")
	}
}

func TestRenderStateIsBounded(t *testing.T) {
	runner := &fakeRunner{eventsFor: func(string) []Event {
		events := make([]Event, 0, maxBufferedEvents/4)
		for range maxBufferedEvents / 4 {
			events = append(events, Event{Kind: "tool.started", Author: "x"})
		}
		events = append(events, Event{Kind: "turn.completed"})
		return events
	}}
	console, out, _, cancel := newConsoleForTest(runner, newFakeSessions(), "prompt\n")
	defer cancel()
	if err := console.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty", out.String())
	}
}

func TestBatchEmptyADKCompletionReplacesPartial(t *testing.T) {
	runner := &fakeRunner{eventsFor: func(string) []Event {
		return []Event{
			{Kind: "model.delta", Payload: delta("stale partial")},
			{Kind: "adk_event", Payload: json.RawMessage(`{"content":{"role":"model","parts":[]},"partial":false}`)},
			{Kind: "turn.completed"},
		}
	}}
	console, out, _, cleanup := newConsoleForTest(runner, newFakeSessions(), "prompt\n")
	defer cleanup()
	if err := console.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want empty authoritative completion", out.String())
	}
}

func TestBatchMergeMaintainsByteBudget(t *testing.T) {
	chunk := strings.Repeat("x", 600*1024)
	stream := []Event{
		{Kind: "adk_event", Payload: json.RawMessage(`{"content":{"role":"model","parts":[{"text":"` + chunk + `"}]},"partial":true}`)},
		{Kind: "model.delta", Payload: delta(chunk)},
		{Kind: "adk_event", Payload: json.RawMessage(`{"content":{"role":"model","parts":[{"text":"` + chunk + `"}]},"partial":true}`)},
	}
	var retained int
	var projected []Event
	for _, event := range stream {
		projected, retained = appendRenderEvent(projected, event, retained)
	}
	projected, retained = appendRenderEvent(projected, Event{Kind: "model.delta", Payload: delta(chunk)}, retained)
	if retained > maxBatchStreamBytes {
		t.Fatalf("retained = %d, want <= %d", retained, maxBatchStreamBytes)
	}
	var payloadBytes int
	for _, event := range projected {
		payloadBytes += len(event.Payload)
	}
	if payloadBytes > maxBatchStreamBytes {
		t.Fatalf("payload bytes = %d, want <= %d", payloadBytes, maxBatchStreamBytes)
	}
}

func TestBatchADKToolProgressStaysOnDiagnostics(t *testing.T) {
	runner := &fakeRunner{eventsFor: func(string) []Event {
		return []Event{
			{Kind: "adk_event", Payload: json.RawMessage(`{"content":{"role":"model","parts":[{"functionCall":{"name":"search"}},{"functionResponse":{"name":"search"}}]},"longRunningToolIds":["tool-1"],"partial":true}`)},
			{Kind: "turn.completed"},
		}
	}}
	console, out, diag, cleanup := newConsoleForTest(runner, newFakeSessions(), "prompt\n")
	defer cleanup()
	if err := console.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want no tool progress", out.String())
	}
	for _, want := range []string{"tool requested: search", "tool completed: search", "long-running tool active"} {
		if !strings.Contains(diag.String(), want) {
			t.Errorf("stderr = %q, missing %q", diag.String(), want)
		}
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("closed output") }

func TestOutputWriteFailureIsReturned(t *testing.T) {
	runner := &fakeRunner{eventsFor: func(string) []Event {
		return []Event{{Kind: "model.delta", Payload: delta("reply")}, {Kind: "turn.completed"}}
	}}
	console := NewConsole(runner, newFakeSessions(), PlainRenderer{}, bytes.NewBufferString("prompt\n"), failingWriter{}, &bytes.Buffer{}, Config{
		MaxInputBytes:       100,
		SecondInterruptTime: time.Second,
	}, "owner")
	if err := console.Run(context.Background()); err == nil {
		t.Fatal("expected output write error")
	}
}

func TestBatchStreamByteBudgetBounded(t *testing.T) {
	// Near-cap ADK payloads would previously retain up to 4 MiB each across
	// 1024 events; the normalized projection plus the aggregate byte budget
	// must keep the retained stream small regardless of payload size.
	big := strings.Repeat("x", 64*1024)
	events := make([]Event, 0, maxBufferedEvents/4)
	for range maxBufferedEvents / 4 {
		events = append(events, Event{Kind: "adk_event", Payload: json.RawMessage(`{"content":{"role":"model","parts":[{"text":"` + big + `"}]},"partial":true}`)})
	}
	runner := &fakeRunner{eventsFor: func(string) []Event {
		return append(events, Event{Kind: "turn.completed"})
	}}
	console, out, _, cleanup := newConsoleForTest(runner, newFakeSessions(), "prompt\n")
	defer cleanup()
	if err := console.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Len() > 2*maxRenderBytes {
		t.Errorf("output = %d bytes, want bounded below the stream input", out.Len())
	}
	var retained int
	var stream []Event
	for _, ev := range events {
		stream, retained = appendRenderEvent(stream, ev, retained)
	}
	if retained > maxBatchStreamBytes {
		t.Fatalf("retained = %d, want <= %d", retained, maxBatchStreamBytes)
	}
	var payloadBytes int
	for _, ev := range stream {
		payloadBytes += len(ev.Payload)
	}
	if payloadBytes > maxBatchStreamBytes {
		t.Fatalf("retained payload bytes = %d, want <= %d", payloadBytes, maxBatchStreamBytes)
	}
}
