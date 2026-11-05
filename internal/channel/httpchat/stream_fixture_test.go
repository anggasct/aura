package httpchat

import (
	"context"
	"encoding/json"
	"iter"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/store"
)

const (
	frameDataMarker = "data: "
	frameDoneMarker = "data: [DONE]"
	frameKeepalives = ": keepalive"
)

type scriptRuntime func(ctx context.Context, yield func(store.RuntimeEvent, error) bool)

func (s scriptRuntime) Run(ctx context.Context, _ *runtime.TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
	return func(yield func(store.RuntimeEvent, error) bool) { s(ctx, yield) }
}

func turnEvent(kind string) store.RuntimeEvent {
	return store.RuntimeEvent{TurnID: testTurnID, Kind: kind}
}

func answerEvent(text string) store.RuntimeEvent {
	event := turnEvent(runtime.EventKindMessageCompleted)
	event.Payload = json.RawMessage(`{"text":` + jsonString(text) + `}`)
	return event
}

func usageEvent(text string) store.RuntimeEvent {
	event := answerEvent(text)
	event.ProviderUsage = json.RawMessage(testUsageJSON)
	return event
}

func failedEvent(code string) store.RuntimeEvent {
	event := turnEvent(runtime.EventKindTurnFailed)
	event.Payload = json.RawMessage(`{"code":"` + code + `","detail":"internal detail with the prompt and secret model output"}`)
	return event
}

func emit(yield func(store.RuntimeEvent, error) bool, events ...store.RuntimeEvent) bool {
	for i := range events {
		if !yield(events[i], nil) {
			return false
		}
	}
	return true
}

func fixedTurn(events ...store.RuntimeEvent) scriptRuntime {
	return func(_ context.Context, yield func(store.RuntimeEvent, error) bool) {
		emit(yield, events...)
	}
}

func streamBody(includeUsage bool, messages ...string) string {
	options := ""
	if includeUsage {
		options = `,"stream_options":{"include_usage":true}`
	}
	return `{"model":"` + testPersona + `","stream":true` + options + `,"messages":[` + strings.Join(messages, ",") + `]}`
}

func defaultStreamBody() string {
	return streamBody(false, userMessage("hi"))
}

func sseFrames(t *testing.T, body string) []string {
	t.Helper()
	if body == "" {
		return nil
	}
	if !strings.HasSuffix(body, "\n\n") {
		t.Fatalf("stream body does not end on an event boundary: %q", body)
	}
	return strings.Split(strings.TrimSuffix(body, "\n\n"), "\n\n")
}

type chunkView struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
	} `json:"usage"`
}

func decodeChunk(t *testing.T, frame string) chunkView {
	t.Helper()
	payload, found := strings.CutPrefix(frame, frameDataMarker)
	if !found {
		t.Fatalf("frame %q is not a data event", frame)
	}
	var decoded chunkView
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("decode chunk %q: %v", payload, err)
	}
	return decoded
}

func contentOf(t *testing.T, frames []string) string {
	t.Helper()
	var text strings.Builder
	for _, frame := range frames {
		if frame == frameDoneMarker || !strings.HasPrefix(frame, frameDataMarker) {
			continue
		}
		decoded := decodeChunk(t, frame)
		for i := range decoded.Choices {
			text.WriteString(decoded.Choices[i].Delta.Content)
		}
	}
	return text.String()
}

func errorEventOf(t *testing.T, frame string) envelope {
	t.Helper()
	payload, found := strings.CutPrefix(frame, frameDataMarker)
	if !found {
		t.Fatalf("frame %q is not a data event", frame)
	}
	var decoded envelope
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("decode error event %q: %v", payload, err)
	}
	return decoded
}

type captureWriter struct {
	mu        sync.Mutex
	header    http.Header
	status    int
	body      strings.Builder
	frames    int
	failAfter int
	deadlines []time.Time
}

func newCaptureWriter() *captureWriter {
	return &captureWriter{header: http.Header{}, failAfter: -1}
}

func (c *captureWriter) Header() http.Header { return c.header }

func (c *captureWriter) WriteHeader(status int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status == 0 {
		c.status = status
	}
}

func (c *captureWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failAfter >= 0 && c.frames >= c.failAfter {
		return 0, http.ErrHandlerTimeout
	}
	c.frames++
	return c.body.Write(p)
}

func (c *captureWriter) Flush() {}

func (c *captureWriter) SetWriteDeadline(deadline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadlines = append(c.deadlines, deadline)
	return nil
}

func (c *captureWriter) snapshot() (status int, body string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status, c.body.String()
}

func (c *captureWriter) armedDeadlines() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Time(nil), c.deadlines...)
}

func (f *chatFixture) serveAsync(req *http.Request, w http.ResponseWriter) <-chan struct{} {
	served := make(chan struct{})
	go func() {
		defer close(served)
		f.handler.ServeHTTP(w, req)
	}()
	return served
}
