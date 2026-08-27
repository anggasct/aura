package terminal

import (
	"bytes"
	"context"
	"io"
	"iter"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTTYConsole(runner Runner, sess *fakeSessions, in string, width int) (console *Console, out, diag *bytes.Buffer, cleanup func()) {
	out = &bytes.Buffer{}
	diag = &bytes.Buffer{}
	_, cancel := context.WithCancel(context.Background())
	console = NewConsole(runner, sess, PlainRenderer{}, bytes.NewBufferString(in), out, diag, Config{
		MaxInputBytes:       1000,
		InMemoryHistory:     100,
		SecondInterruptTime: 2 * time.Second,
	}, "owner")
	console.SetTTY(NewTTYRenderer(TTYOptions{
		Out:     testWriteCloser{Writer: out},
		Width:   func() int { return width },
		Hz:      500,
		Styling: false,
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
	}))
	return console, out, diag, cancel
}

func TestTTYStreamingTurnWritesFinalText(t *testing.T) {
	runner := &fakeRunner{eventsFor: func(string) []Event {
		return []Event{
			{Kind: "model.delta", Payload: delta("stre")},
			{Kind: "model.delta", Payload: delta("amed")},
			{Kind: "message.completed", Payload: completed("final answer")},
			{Kind: "turn.completed"},
		}
	}}
	console, out, _, cleanup := newTTYConsole(runner, newFakeSessions(), "hi\n", 80)
	defer cleanup()
	if err := console.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "final answer") {
		t.Errorf("output = %q, want final text", out.String())
	}
}

func TestTTYFailedTurnReportsFailure(t *testing.T) {
	runner := &fakeRunner{eventsFor: func(string) []Event {
		return []Event{
			{Kind: "model.delta", Payload: delta("partial")},
			{Kind: "turn.failed"},
		}
	}}
	console, out, _, cleanup := newTTYConsole(runner, newFakeSessions(), "x\n", 80)
	defer cleanup()
	err := console.Run(context.Background())
	if err == nil {
		t.Fatal("Run returned nil for failed turn")
	}
	if !strings.Contains(out.String(), "turn failed") {
		t.Errorf("output = %q, want failure frame", out.String())
	}
	if strings.Contains(out.String(), "partial") {
		t.Errorf("output = %q, partial must not survive a failed turn", out.String())
	}
}

func TestTTYCancelledTurnReplacesPartials(t *testing.T) {
	runner := &fakeRunner{eventsFor: func(string) []Event {
		return []Event{{Kind: "model.delta", Payload: delta("partial")}, {Kind: "turn.cancelled"}}
	}}
	console, out, _, cleanup := newTTYConsole(runner, newFakeSessions(), "x\n", 80)
	defer cleanup()
	if err := console.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(out.String(), "partial") || !strings.Contains(out.String(), "turn cancelled") {
		t.Errorf("output = %q, want cancellation without partial", out.String())
	}
}

func TestPlainPeriodIsAnOrdinaryPrompt(t *testing.T) {
	runner := &fakeRunner{eventsFor: func(string) []Event {
		return []Event{{Kind: "turn.completed"}}
	}}
	console, _, _, cleanup := newConsoleForTest(runner, newFakeSessions(), ".\n")
	defer cleanup()
	if err := console.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(runner.calls) != 1 || runner.calls[0].Parts[0].Text != "." {
		t.Errorf("calls = %+v, want the period as a prompt", runner.calls)
	}
}

func TestTTYMultilineEditorGesture(t *testing.T) {
	script := filepath.Join(t.TempDir(), "editor.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'line one\\nline two\\n' > \"$1\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDITOR", script)
	runner := &fakeRunner{eventsFor: func(string) []Event {
		return []Event{{Kind: "turn.completed"}}
	}}
	console, _, _, cleanup := newTTYConsole(runner, newFakeSessions(), ".\n", 80)
	defer cleanup()
	if err := console.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(runner.calls))
	}
	if got := runner.calls[0].Parts[0].Text; got != "line one\nline two" {
		t.Errorf("composed prompt = %q, want both lines", got)
	}
}

func TestTTYEditorGestureBounded(t *testing.T) {
	script := filepath.Join(t.TempDir(), "editor.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nhead -c 4096 /dev/zero | tr '\\0' 'a' > \"$1\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDITOR", script)
	runner := &fakeRunner{eventsFor: func(string) []Event {
		return []Event{{Kind: "turn.completed"}}
	}}
	// The composition cap is far below the 4096-byte draft.
	console, _, diag, cleanup := newTTYConsole(runner, newFakeSessions(), ".\n", 80)
	defer cleanup()
	if err := console.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Errorf("calls = %d, oversized draft must not submit", len(runner.calls))
	}
	if !strings.Contains(diag.String(), "exceeds the configured maximum") {
		t.Errorf("stderr = %q, want bound diagnostic", diag.String())
	}
}

func TestTTYEditorUnavailableDegrades(t *testing.T) {
	t.Setenv("EDITOR", "definitely-missing-editor-bin")
	runner := &fakeRunner{eventsFor: func(string) []Event {
		return []Event{{Kind: "turn.completed"}}
	}}
	console, _, diag, cleanup := newTTYConsole(runner, newFakeSessions(), ".\n", 80)
	defer cleanup()
	if err := console.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Errorf("calls = %d, failed editor must not submit", len(runner.calls))
	}
	if !strings.Contains(diag.String(), "editor") {
		t.Errorf("stderr = %q, want editor diagnostic", diag.String())
	}
}

func TestPastedLinesEachBecomeTurns(t *testing.T) {
	runner := &fakeRunner{eventsFor: func(string) []Event {
		return []Event{{Kind: "turn.completed"}}
	}}
	paste := strings.Repeat("prompt\n", 50)
	console, _, _, cleanup := newConsoleForTest(runner, newFakeSessions(), paste)
	defer cleanup()
	if err := console.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(runner.calls) != 50 {
		t.Errorf("calls = %d, want one per pasted line", len(runner.calls))
	}
}

func TestTTYClearUsesRenderer(t *testing.T) {
	runner := &fakeRunner{eventsFor: func(string) []Event {
		return []Event{{Kind: "turn.completed"}}
	}}
	console, out, _, cleanup := newTTYConsole(runner, newFakeSessions(), "/clear\n", 80)
	defer cleanup()
	if err := console.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Len() != 1 || out.String() != "\n" {
		t.Errorf("output = %q, want the unstyled clear degrade", out.String())
	}
}

func TestEditorCommandContextCancels(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	r := NewTTYRenderer(TTYOptions{
		Out:     testWriteCloser{Writer: &bytes.Buffer{}},
		Width:   func() int { return 80 },
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Styling: false,
	})
	ctx, cancel := context.WithCancel(context.Background())
	slow := filepath.Join(t.TempDir(), "slow.sh")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\ntrap 'exit' TERM\nwhile :; do sleep 0.1; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDITOR", slow)
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, _, err := r.Compose(ctx, 1024)
	if err == nil {
		t.Fatal("cancelled editor compose must fail")
	}
}

type blockingTTYRunner struct {
	cancelled chan struct{}
	once      sync.Once
}

func (r *blockingTTYRunner) Run(ctx context.Context, _ *Request) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		yield(Event{Kind: "model.delta", Payload: delta("partial")}, nil)
		<-ctx.Done()
		r.once.Do(func() { close(r.cancelled) })
	}
}

func TestTTYClosedOutputCancelsProducer(t *testing.T) {
	runner := &blockingTTYRunner{cancelled: make(chan struct{})}
	console := NewConsole(runner, newFakeSessions(), PlainRenderer{}, bytes.NewBufferString("x\n"), failingWriter{}, &bytes.Buffer{}, Config{
		MaxInputBytes:       100,
		SecondInterruptTime: time.Second,
	}, "owner")
	console.SetTTY(NewTTYRenderer(TTYOptions{Out: testWriteCloser{Writer: failingWriter{}}, Width: func() int { return 80 }, Hz: 500}))
	done := make(chan error, 1)
	go func() { done <- console.Run(context.Background()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run returned nil for closed output")
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after closed output")
	}
	select {
	case <-runner.cancelled:
	case <-time.After(time.Second):
		t.Fatal("runner was not cancelled after closed output")
	}
}

type blockingConsoleWriter struct {
	started    chan struct{}
	release    chan struct{}
	finished   chan struct{}
	once       sync.Once
	finishOnce sync.Once
	closeOnce  sync.Once
}

func (w *blockingConsoleWriter) finish() {
	w.finishOnce.Do(func() { close(w.finished) })
}

func (w *blockingConsoleWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	w.finish()
	return len(p), nil
}

func (w *blockingConsoleWriter) WriteContext(ctx context.Context, p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	select {
	case <-w.release:
		w.finish()
		return len(p), nil
	case <-ctx.Done():
		w.finish()
		return 0, ctx.Err()
	}
}

func (w *blockingConsoleWriter) Close() error {
	w.closeOnce.Do(func() { close(w.release) })
	return nil
}

func TestTTYSlowOutputDoesNotHangConsole(t *testing.T) {
	runner := &blockingTTYRunner{cancelled: make(chan struct{})}
	out := &blockingConsoleWriter{
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		finished: make(chan struct{}),
	}
	console := NewConsole(runner, newFakeSessions(), PlainRenderer{}, bytes.NewBufferString("x\n"), out, &bytes.Buffer{}, Config{
		MaxInputBytes:       100,
		SecondInterruptTime: time.Second,
	}, "owner")
	console.SetTTY(NewTTYRenderer(TTYOptions{Out: out, Width: func() int { return 80 }, Hz: 500}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- console.Run(ctx) }()
	select {
	case <-out.started:
	case <-time.After(time.Second):
		t.Fatal("paint did not reach the blocking writer")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run returned nil for a stalled output")
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("Run remained blocked on stalled output")
	}
	_ = out.Close()
	select {
	case <-out.finished:
	case <-time.After(time.Second):
		t.Fatal("blocked writer did not release")
	}
	select {
	case <-runner.cancelled:
	case <-time.After(time.Second):
		t.Fatal("runner was not cancelled")
	}
}

// TestEditorGetsExclusiveStdin proves the line reader is paused while the
// editor gesture runs: lines arriving during composition are consumed only
// after the editor exits, so editor keystrokes cannot be stolen mid-draft.
func TestEditorGetsExclusiveStdin(t *testing.T) {
	script := filepath.Join(t.TempDir(), "editor.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 0.3\nprintf 'composed line\\n' > \"$1\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDITOR", script)
	runner := &fakeRunner{eventsFor: func(string) []Event {
		return []Event{{Kind: "turn.completed"}}
	}}
	pr, pw := io.Pipe()
	out := &bytes.Buffer{}
	diag := &bytes.Buffer{}
	console := NewConsole(runner, newFakeSessions(), PlainRenderer{}, pr, out, diag, Config{
		MaxInputBytes:       1000,
		SecondInterruptTime: 2 * time.Second,
	}, "owner")
	console.SetTTY(NewTTYRenderer(TTYOptions{Out: testWriteCloser{Writer: out}, Width: func() int { return 80 }, Hz: 500, Stdin: os.Stdin, Stdout: os.Stdout}))
	done := make(chan error, 1)
	go func() { done <- console.Run(context.Background()) }()
	if _, err := pw.Write([]byte(".\n")); err != nil {
		t.Fatal(err)
	}
	// While the editor holds stdin (0.3s), a line written to the pipe must
	// not be consumed as a prompt by the paused reader.
	time.Sleep(100 * time.Millisecond)
	if _, err := pw.Write([]byte("during-editor\n/exit\n")); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	_ = pw.Close()
	if len(runner.calls) == 0 || runner.calls[0].Parts[0].Text != "composed line" {
		t.Fatalf("calls = %+v, want the composed draft as the turn input", runner.calls)
	}
	if len(runner.calls) < 2 || runner.calls[1].Parts[0].Text != "during-editor" {
		t.Errorf("calls = %d, want the during-editor line consumed after resume", len(runner.calls))
	}
}

func TestReadLinesDoesNotPrefetchWhilePaused(t *testing.T) {
	r := strings.NewReader("first\nsecond\n")
	lines, pause, resume, stop := readLines(context.Background(), r, 1000, nil)
	defer stop()
	result := <-lines
	if result.err != nil || result.line != "first" {
		t.Fatalf("result = %+v, want first line", result)
	}
	pause()
	close(result.ack)
	if remaining := r.Len(); remaining != len("second\n") {
		t.Fatalf("reader has %d bytes remaining while paused, want %d", remaining, len("second\n"))
	}
	resume()
	result = <-lines
	if result.err != nil || result.line != "second" {
		t.Fatalf("result = %+v, want second line", result)
	}
	close(result.ack)
}
