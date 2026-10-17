package runtimeadk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/model"
	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/runtime/ingress"
	"github.com/anggasct/aura/internal/store"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

var captureID atomic.Int64

func captureResponsesServer(t *testing.T, mu *sync.Mutex, bodies *[][]byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			t.Errorf("read capture body: %v", err)
		}
		mu.Lock()
		*bodies = append(*bodies, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-5","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
	}))
}

func registerCaptureModel(t *testing.T, serverURL string) string {
	t.Helper()
	name := fmt.Sprintf("capture-model-%d", captureID.Add(1))
	adkmodel.Register("^"+name+"$", func(context.Context, string) (adkmodel.LLM, error) {
		return model.NewOpenAIResponsesAdapter(name, serverURL, "capture-key", 0)
	})
	return name
}

func seedUserEvent(t *testing.T, events store.EventStore, sessionID, turnID, eventID, text string, seq uint64) {
	t.Helper()
	ev := &session.Event{
		ID:        eventID,
		Timestamp: time.Now().UTC(),
		Author:    "user",
	}
	ev.Content = genai.NewContentFromText(text, genai.RoleUser)
	re, err := store.RuntimeEventFromADK(sessionID, turnID, ev)
	if err != nil {
		t.Fatalf("RuntimeEventFromADK: %v", err)
	}
	re.Sequence = seq
	if err := events.Append(context.Background(), &re); err != nil {
		t.Fatalf("Append seed event: %v", err)
	}
}

func seedAssistantEvent(t *testing.T, events store.EventStore, sessionID, turnID, eventID, text string, seq uint64) {
	t.Helper()
	ev := &session.Event{
		ID:        eventID,
		Timestamp: time.Now().UTC(),
		Author:    "aura",
	}
	ev.Content = genai.NewContentFromText(text, genai.RoleModel)
	re, err := store.RuntimeEventFromADK(sessionID, turnID, ev)
	if err != nil {
		t.Fatalf("RuntimeEventFromADK: %v", err)
	}
	re.Sequence = seq
	if err := events.Append(context.Background(), &re); err != nil {
		t.Fatalf("Append seed event: %v", err)
	}
}

func TestTurnCarriesTriggeringMessage(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	srv := captureResponsesServer(t, &mu, &bodies)
	defer srv.Close()

	modelName := registerCaptureModel(t, srv.URL)
	db, sessions, events := newSessionTestDB(t)
	executor, err := NewADKExecutor("aura", modelName, sessions, events, &fakeBroker{}, nil, nil)
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	mustCreateSession(t, db, "session-input-1")

	req := &runtime.TurnRequest{
		TurnID:      "turn-input-1",
		SessionID:   "session-input-1",
		PrincipalID: "user-1",
		Origin:      runtime.OriginTerminal,
		Parts:       []runtimeingress.InputPart{{Text: "hello turn input"}},
	}
	for _, err := range executor.Execute(context.Background(), req) {
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) == 0 {
		t.Fatal("no model requests captured")
	}
	var payload struct {
		Input []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal(bodies[0], &payload); err != nil {
		t.Fatalf("unmarshal captured request: %v", err)
	}
	if len(payload.Input) == 0 {
		t.Fatalf("captured request has no input: %s", string(bodies[0]))
	}
	joined, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal captured input: %v", err)
	}
	if !strings.Contains(string(joined), "hello turn input") {
		t.Errorf("captured input %s does not carry triggering text", string(joined))
	}
}

func TestTurnHistoryOrderingExactlyOnce(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	srv := captureResponsesServer(t, &mu, &bodies)
	defer srv.Close()

	modelName := registerCaptureModel(t, srv.URL)
	db, sessions, events := newSessionTestDB(t)
	executor, err := NewADKExecutor("aura", modelName, sessions, events, &fakeBroker{}, nil, nil)
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	mustCreateSession(t, db, "session-input-2")
	seedUserEvent(t, events, "session-input-2", "turn-prior-1", "event-prior-1", "prior-one", 1)
	seedAssistantEvent(t, events, "session-input-2", "turn-prior-1", "event-prior-2", "prior-two", 2)

	req := &runtime.TurnRequest{
		TurnID:      "turn-input-2",
		SessionID:   "session-input-2",
		PrincipalID: "user-1",
		Origin:      runtime.OriginTerminal,
		Parts:       []runtimeingress.InputPart{{Text: "current-three"}},
	}
	for _, err := range executor.Execute(context.Background(), req) {
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) == 0 {
		t.Fatal("no model requests captured")
	}
	body := string(bodies[0])
	first := strings.Index(body, "prior-one")
	second := strings.Index(body, "prior-two")
	third := strings.Index(body, "current-three")
	if first < 0 || second < 0 || third < 0 {
		t.Fatalf("captured input missing ordered texts: %s", body)
	}
	if first >= second || second >= third {
		t.Errorf("history order wrong: prior-one=%d prior-two=%d current=%d in %s", first, second, third, body)
	}
	if strings.Count(body, "current-three") != 1 {
		t.Errorf("triggering message appears %d times, want exactly once in %s", strings.Count(body, "current-three"), body)
	}
}

func TestTurnToolLoopShapesPreserved(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	srv := captureResponsesServer(t, &mu, &bodies)
	defer srv.Close()

	modelName := registerCaptureModel(t, srv.URL)
	db, sessions, events := newSessionTestDB(t)
	sample, _ := newRecordingTool(t)
	executor, err := NewADKExecutor("aura", modelName, sessions, events, &fakeBroker{}, []tool.Tool{sample}, nil)
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	mustCreateSession(t, db, "session-input-4")
	seedToolLoopHistory(t, events, "session-input-4")

	req := &runtime.TurnRequest{
		TurnID:      "turn-input-4",
		SessionID:   "session-input-4",
		PrincipalID: "user-1",
		Origin:      runtime.OriginTerminal,
		Parts:       []runtimeingress.InputPart{{Text: "follow-up after tools"}},
	}
	for _, err := range executor.Execute(context.Background(), req) {
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) == 0 {
		t.Fatal("no model requests captured")
	}
	body := string(bodies[0])
	if !strings.Contains(body, "input_text") {
		t.Errorf("captured input lacks text shape: %s", body)
	}
	if !strings.Contains(body, "function_call_output") {
		t.Errorf("captured input lacks tool result shape: %s", body)
	}
	if !strings.Contains(body, "follow-up after tools") {
		t.Errorf("captured input lacks current message: %s", body)
	}
}

func seedToolLoopHistory(t *testing.T, events store.EventStore, sessionID string) {
	t.Helper()
	seedUserEvent(t, events, sessionID, "turn-prior-tool", "event-tool-1", "run the tool", 1)
	callEvent := &session.Event{
		ID:        "event-tool-2",
		Timestamp: time.Now().UTC(),
		Author:    "aura",
	}
	callEvent.Content = &genai.Content{
		Role: genai.RoleModel,
		Parts: []*genai.Part{{
			FunctionCall: &genai.FunctionCall{
				ID:   "call-1",
				Name: "sample_tool",
				Args: map[string]any{"query": "x"},
			},
		}},
	}
	re, err := store.RuntimeEventFromADK(sessionID, "turn-prior-tool", callEvent)
	if err != nil {
		t.Fatalf("RuntimeEventFromADK call: %v", err)
	}
	re.Sequence = 2
	if err := events.Append(context.Background(), &re); err != nil {
		t.Fatalf("Append call: %v", err)
	}
	responseEvent := &session.Event{
		ID:        "event-tool-3",
		Timestamp: time.Now().UTC(),
		Author:    "user",
	}
	responseEvent.Content = &genai.Content{
		Role: genai.RoleUser,
		Parts: []*genai.Part{{
			FunctionResponse: &genai.FunctionResponse{
				ID:       "call-1",
				Name:     "sample_tool",
				Response: map[string]any{"ok": true},
			},
		}},
	}
	re, err = store.RuntimeEventFromADK(sessionID, "turn-prior-tool", responseEvent)
	if err != nil {
		t.Fatalf("RuntimeEventFromADK response: %v", err)
	}
	re.Sequence = 3
	if err := events.Append(context.Background(), &re); err != nil {
		t.Fatalf("Append response: %v", err)
	}
}

func TestTurnEmptyGuardEmitsNoRequest(t *testing.T) {
	fake := &fakeADKModel{answer: "unused", tokens: 1}
	modelName := registerFakeModel(t, fake)
	executor, db, _ := newADKTestExecutor(t, modelName, &fakeBroker{})
	mustCreateSession(t, db, "session-input-3")

	req := &runtime.TurnRequest{
		TurnID:      "turn-input-3",
		SessionID:   "session-input-3",
		PrincipalID: "user-1",
		Origin:      runtime.OriginTerminal,
		Parts:       []runtimeingress.InputPart{{Text: "   "}},
	}
	var gotErr error
	for _, err := range executor.Execute(context.Background(), req) {
		if err != nil {
			gotErr = err
		}
	}
	if gotErr == nil {
		t.Fatal("empty turn produced no error, want typed invalid input")
	}
	if code, ok := runtime.CodeOf(gotErr); !ok || code != runtime.ErrorCodeInvalidArgument {
		t.Fatalf("CodeOf(%v) = %q, want invalid_argument", gotErr, code)
	}
	if fake.callCount != 0 {
		t.Errorf("model calls = %d, want 0 (no request emitted)", fake.callCount)
	}
}
