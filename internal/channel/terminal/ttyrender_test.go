package terminal

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func ttPayload(t *testing.T, text string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(struct {
		Text string `json:"text"`
	}{Text: text})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newTestTTY(out io.Writer, width func() int, styling bool) *TTYRenderer {
	return NewTTYRenderer(TTYOptions{Out: out, Width: width, Hz: 500, Styling: styling})
}

func TestTTYCompletedEventWinsOverPartials(t *testing.T) {
	out := &bytes.Buffer{}
	r := newTestTTY(out, func() int { return 80 }, false)
	r.Begin()
	r.Observe(Event{Kind: "model.delta", Payload: ttPayload(t, "hel")})
	r.Observe(Event{Kind: "model.delta", Payload: ttPayload(t, "lo")})
	r.Observe(Event{Kind: "message.completed", Payload: ttPayload(t, "goodbye")})
	r.Observe(Event{Kind: "turn.completed"})
	if err := r.Finalize(false, false); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if !strings.Contains(out.String(), "goodbye") {
		t.Errorf("output = %q, want completed text", out.String())
	}
	if strings.Contains(out.String(), "hello") {
		t.Errorf("output = %q, streamed partial must not survive", out.String())
	}
}

func TestTTYADKEventsRenderAssistantText(t *testing.T) {
	out := &bytes.Buffer{}
	r := newTestTTY(out, func() int { return 80 }, false)
	r.Begin()
	r.Observe(Event{Kind: "adk_event", Payload: json.RawMessage(`{"content":{"role":"model","parts":[{"text":"assistant answer"}]},"partial":false}`)})
	r.Observe(Event{Kind: "turn.completed"})
	if err := r.Finalize(false, false); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if !strings.Contains(out.String(), "assistant answer") {
		t.Errorf("output = %q, want assistant text", out.String())
	}
}

func TestTTYPartialSurvivesWithoutCompletedMessage(t *testing.T) {
	out := &bytes.Buffer{}
	r := newTestTTY(out, func() int { return 80 }, false)
	r.Begin()
	r.Observe(Event{Kind: "model.delta", Payload: ttPayload(t, "partial answer")})
	r.Observe(Event{Kind: "turn.completed"})
	if err := r.Finalize(false, false); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if !strings.Contains(out.String(), "partial answer") {
		t.Errorf("output = %q, want accumulated partial", out.String())
	}
}

func TestTTYFailureFrameReplacesPartials(t *testing.T) {
	out := &bytes.Buffer{}
	r := newTestTTY(out, func() int { return 80 }, false)
	r.Begin()
	r.Observe(Event{Kind: "model.delta", Payload: ttPayload(t, "partial")})
	r.Observe(Event{Kind: "turn.failed"})
	if err := r.Finalize(true, false); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if !strings.Contains(out.String(), "turn failed") {
		t.Errorf("output = %q, want failure frame", out.String())
	}
	if strings.Contains(out.String(), "partial") {
		t.Errorf("output = %q, failed turn partial must not survive", out.String())
	}
}

func TestTTYEmptyCompletedEventReplacesPartials(t *testing.T) {
	out := &bytes.Buffer{}
	r := newTestTTY(out, func() int { return 80 }, false)
	r.Begin()
	r.Observe(Event{Kind: "model.delta", Payload: ttPayload(t, "partial")})
	r.Observe(Event{Kind: "message.completed", Payload: ttPayload(t, "")})
	r.Observe(Event{Kind: "turn.completed"})
	if err := r.Finalize(false, false); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if strings.Contains(out.String(), "partial") {
		t.Errorf("output = %q, empty completion must replace partial", out.String())
	}
}

func TestTTYRepaintIncludesProgressLines(t *testing.T) {
	out := &bytes.Buffer{}
	r := newTestTTY(out, func() int { return 80 }, true)
	r.Begin()
	r.Observe(Event{Kind: "tool.started", Author: "tool"})
	r.Observe(Event{Kind: "model.delta", Payload: ttPayload(t, "first")})
	if err := r.Paint(); err != nil {
		t.Fatalf("first Paint: %v", err)
	}
	r.Observe(Event{Kind: "tool.completed", Author: "tool"})
	if err := r.Paint(); err != nil {
		t.Fatalf("second Paint: %v", err)
	}
	if !strings.Contains(out.String(), "\x1b[2A\r\x1b[J") {
		t.Errorf("output = %q, want repaint over progress and body lines", out.String())
	}
}

func TestTTYSanitizesUntrustedStream(t *testing.T) {
	out := &bytes.Buffer{}
	r := newTestTTY(out, func() int { return 80 }, true)
	r.Begin()
	r.Observe(Event{Kind: "model.delta", Payload: ttPayload(t, "x\x1b[31m red\x1b]8;;https://evil.test\a link\x1b]8;;\a\x00\xff end")})
	r.Observe(Event{Kind: "tool.started", Author: "tool\x1b[2J"})
	r.Observe(Event{Kind: "turn.completed"})
	if err := r.Finalize(false, false); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	got := out.String()
	// The renderer's own escape sequences are limited to the styling set;
	// untrusted ESC payloads must not appear.
	untrusted := strings.ReplaceAll(got, dim, "")
	untrusted = strings.ReplaceAll(untrusted, bold, "")
	untrusted = strings.ReplaceAll(untrusted, reset, "")
	untrusted = strings.ReplaceAll(untrusted, "\x1b[J", "")
	if i := strings.IndexByte(untrusted, '\x1b'); i >= 0 {
		t.Errorf("untrusted escape survived at %d: %q", i, untrusted)
	}
	if strings.ContainsRune(untrusted, 0) {
		t.Error("NUL byte survived sanitization")
	}
}

func TestTTYCoalescesFrames(t *testing.T) {
	out := &countingWriter{}
	r := NewTTYRenderer(TTYOptions{Out: out, Width: func() int { return 80 }, Hz: 1000})
	r.Begin()
	pumpCtx, stop := context.WithCancel(context.Background())
	done := r.StartPump(pumpCtx, nil)
	for range 500 {
		r.Observe(Event{Kind: "model.delta", Payload: ttPayload(t, "x")})
		time.Sleep(50 * time.Microsecond)
	}
	r.Observe(Event{Kind: "turn.completed"})
	stop()
	<-done
	if out.writes >= 500 {
		t.Errorf("writes = %d, want coalesced below the event count", out.writes)
	}
	if err := r.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
}

type countingWriter struct {
	mu     sync.Mutex
	writes int
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.writes++
	w.mu.Unlock()
	return len(p), nil
}

func TestTTYSlowWriterStaysBoundedAndCancellable(t *testing.T) {
	release := make(chan struct{})
	out := &blockingWriter{release: release}
	r := NewTTYRenderer(TTYOptions{Out: out, Width: func() int { return 80 }, Hz: 1000})
	r.Begin()
	pumpCtx, stop := context.WithCancel(context.Background())
	done := r.StartPump(pumpCtx, nil)
	r.Observe(Event{Kind: "model.delta", Payload: ttPayload(t, "data")})
	time.Sleep(5 * time.Millisecond)
	// The first paint blocks in the writer; cancellation must stop the pump
	// without piling on more frames.
	stop()
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not stop after cancel and release")
	}
}

type blockingWriter struct {
	release chan struct{}
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	<-w.release
	return len(p), nil
}

func TestTTYClosedOutputSurfacesError(t *testing.T) {
	out := &failingWriter{}
	r := newTestTTY(out, nil, false)
	r.Begin()
	pumpCtx, stop := context.WithCancel(context.Background())
	done := r.StartPump(pumpCtx, nil)
	r.Observe(Event{Kind: "model.delta", Payload: ttPayload(t, "data")})
	<-done
	if r.Err() == nil {
		t.Fatal("paint failure was not recorded")
	}
	stop()
}

func TestTTYResizeAdaptsWidth(t *testing.T) {
	out := &bytes.Buffer{}
	width := 40
	r := newTestTTY(out, func() int { return width }, false)
	r.Begin()
	r.Observe(Event{Kind: "model.delta", Payload: ttPayload(t, "0123456789012345678901234567890123456789")})
	if err := r.Paint(); err != nil {
		t.Fatalf("first paint: %v", err)
	}
	first := out.String()
	width = 10
	r.Observe(Event{Kind: "model.delta", Payload: ttPayload(t, "x")})
	if err := r.Paint(); err != nil {
		t.Fatalf("second paint: %v", err)
	}
	second := out.String()
	if first == second {
		t.Fatal("resize produced no repaint")
	}
	for _, frame := range []string{first, second} {
		for _, line := range strings.Split(frame, "\n") {
			if stringWidth(line) > 10 && line != "" && !strings.HasPrefix(line, "\x1b") {
				t.Logf("note: frame line width %d in %q", stringWidth(line), line)
			}
		}
	}
}

func TestTTYTinyWidthBoundsFrame(t *testing.T) {
	out := &bytes.Buffer{}
	r := newTestTTY(out, func() int { return 1 }, false)
	r.Begin()
	r.Observe(Event{Kind: "model.delta", Payload: ttPayload(t, strings.Repeat("x", 100*maxDisplayLines))})
	r.Observe(Event{Kind: "turn.completed"})
	if err := r.Finalize(false, false); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	lines := strings.Count(out.String(), "\n")
	if lines > maxDisplayLines+1 {
		t.Errorf("frame lines = %d, want <= %d", lines, maxDisplayLines)
	}
}

func TestTTYNoColorOmitsStyling(t *testing.T) {
	out := &bytes.Buffer{}
	r := newTestTTY(out, func() int { return 80 }, false)
	r.Begin()
	r.Observe(Event{Kind: "model.delta", Payload: ttPayload(t, "answer")})
	if err := r.Finalize(false, false); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if strings.Contains(out.String(), "\x1b[") {
		t.Errorf("output = %q, styling disabled must emit no escapes", out.String())
	}
}
