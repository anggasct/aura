package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"iter"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/config"
	contextpkg "github.com/anggasct/aura/internal/context"
	"github.com/anggasct/aura/internal/store"
)

func contextRow(kind, payload string) *store.RuntimeEvent {
	return &store.RuntimeEvent{
		ID: "evt-1", SessionID: "sess-1", Sequence: 4, TurnID: "t1",
		InvocationID: "inv-1", Author: "owner", Kind: kind, SchemaVersion: 1,
		Payload: json.RawMessage(payload), CreatedAt: time.Now().UTC(),
	}
}

func TestMapContextEventText(t *testing.T) {
	row := contextRow("message.completed", `{"content":{"parts":[{"text":"hello "},{"text":"world"}]}}`)
	event := mapContextEvent(row)
	if event.Text != "hello world" || event.ID != "evt-1" || event.Sequence != 4 || event.TurnID != "t1" {
		t.Errorf("event = %+v", event)
	}
}

func TestMapContextEventFallbacks(t *testing.T) {
	cases := map[string]struct {
		kind    string
		payload string
		text    string
	}{
		"text field":   {"message.completed", `{"text":"plain"}`, "plain"},
		"invalid json": {"message.completed", "{broken", ""},
		"partial":      {"message.completed", `{"content":{"parts":[{"text":"half"}]},"partial":true}`, ""},
		"empty":        {"turn.accepted", `{}`, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := mapContextEvent(contextRow(tc.kind, tc.payload)); got.Text != tc.text {
				t.Errorf("text = %q", got.Text)
			}
		})
	}
}

func TestMapContextEventCorrelation(t *testing.T) {
	row := contextRow("tool.requested", `{"tool_call_id":"call-1","effect_intent_id":"intent-1","approval_id":"appr-1"}`)
	if got := mapContextEvent(row); got.CorrelationID != "call-1" {
		t.Errorf("correlation = %q", got.CorrelationID)
	}
	row = contextRow("channel.requested", `{"effect_intent_id":"intent-2"}`)
	if got := mapContextEvent(row); got.CorrelationID != "intent-2" {
		t.Errorf("correlation = %q", got.CorrelationID)
	}
	row = contextRow("approval.required", `{"approval_id":"appr-3"}`)
	if got := mapContextEvent(row); got.CorrelationID != "appr-3" {
		t.Errorf("correlation = %q", got.CorrelationID)
	}
}

func TestMapContextEventToolResults(t *testing.T) {
	row := contextRow("tool.completed", `{"text":"output"}`)
	if got := mapContextEvent(row); !got.ToolResult {
		t.Errorf("tool.completed not flagged: %+v", got)
	}
	row = contextRow("adk_event", `{"content":{"parts":[{"functionResponse":{"id":"call-9","name":"run"}}]}}`)
	got := mapContextEvent(row)
	if !got.ToolResult || got.CorrelationID != "call-9" {
		t.Errorf("function response not detected: %+v", got)
	}
	row = contextRow("message.completed", `{"text":"plain"}`)
	if got := mapContextEvent(row); got.ToolResult {
		t.Errorf("plain message flagged: %+v", got)
	}
}

func TestMapContextEventMediaType(t *testing.T) {
	row := contextRow("adk_event", `{"content":{"parts":[{"inlineData":{"mimeType":"image/png"}}]}}`)
	if got := mapContextEvent(row); got.MediaType != "image/png" {
		t.Errorf("media = %q", got.MediaType)
	}
	row = contextRow("adk_event", `{"content":{"parts":[{"fileData":{"mimeType":"audio/ogg"}}]}}`)
	if got := mapContextEvent(row); got.MediaType != "audio/ogg" {
		t.Errorf("media = %q", got.MediaType)
	}
}

func writeContextCLIConfig(t *testing.T, extra string) (cfgPath, dataRoot string) {
	t.Helper()
	dataRoot = t.TempDir()
	path := dataRoot + "/config.yaml"
	content := `version: 1
models:
  definitions:
    primary:
      protocol: openai_chat_compat
      model: test-model
      api_key_env: AURA_TEST_MODEL_KEY
      capabilities:
        streaming: true
        tools: true
        context_tokens: 200000
        tokenizer: test
storage:
  path: ` + dataRoot + `
tools:
  workspace: ` + dataRoot + `
skills:
  roots: [` + dataRoot + `]
context:
  recent_complete_turns: 5
` + extra
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, dataRoot
}

func openContextTestDB(t *testing.T, cfgPath string) *sql.DB {
	t.Helper()
	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	db, err := openStorage(t.Context(), loaded.Config)
	if err != nil {
		t.Fatalf("openStorage: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seedContextSession(t *testing.T, db *sql.DB) {
	t.Helper()
	sessions := store.NewSessionService(db)
	if err := sessions.Create(t.Context(), &store.Session{ID: "sess-1", OwnerID: "owner-1", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), Metadata: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	events := store.NewEventStore(db)
	for i, text := range []string{"first", "second"} {
		if _, err := events.AppendSequenced(t.Context(), "sess-1", &store.RuntimeEvent{
			ID: "evt-" + string(rune('a'+i)), SessionID: "sess-1", TurnID: "t1",
			InvocationID: "inv-old", Author: "owner", Kind: "message.completed",
			SchemaVersion: 1, Payload: json.RawMessage(`{"text":"` + text + `"}`), CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
}

func TestContextSummaryStoreRoundTrip(t *testing.T) {
	cfgPath, _ := writeContextCLIConfig(t, "")
	db := openContextTestDB(t, cfgPath)
	seedContextSession(t, db)
	adapter := newContextSummaryStore(store.NewSessionService(db), store.NewEventStore(db))
	listed, err := adapter.ListSummaries(t.Context(), "sess-1")
	if err != nil {
		t.Fatalf("ListSummaries: %v", err)
	}
	if len(listed) != 0 {
		t.Errorf("listed = %+v", listed)
	}
	payload := []byte(`{"source":{"start_sequence":1,"end_sequence":2,"event_ids":[],"digest":"abc"},"producer":{"provider":"p","model":"m","model_version":"","prompt_version":"v1","prompt_digest":"d"},"tokens":{"source":4,"summary":2,"accounting":"exact"},"trust":"derived_untrusted","summary":{"goals":["g"],"decisions":[],"constraints":[],"open_work":[],"facts":[]},"generated_at":"2026-09-14T00:00:00Z","kind":"context.summary.v1","schema_version":1}`)
	record := &contextpkg.SummaryRecord{ID: "ctxsum-test", SessionID: "sess-1", StartSequence: 1, EndSequence: 2, TurnID: "t1", Payload: payload, CreatedAt: time.Now().UTC()}
	if err := adapter.UpsertSummary(t.Context(), record); err != nil {
		t.Fatalf("UpsertSummary: %v", err)
	}
	if err := adapter.UpsertSummary(t.Context(), record); err != nil {
		t.Fatalf("UpsertSummary idempotent: %v", err)
	}
	listed, err = adapter.ListSummaries(t.Context(), "sess-1")
	if err != nil {
		t.Fatalf("ListSummaries: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != "ctxsum-test" || listed[0].StartSequence != 1 || listed[0].EndSequence != 2 || listed[0].TurnID != "t1" {
		t.Errorf("listed = %+v", listed)
	}
	var nilCtx context.Context
	if err := adapter.UpsertSummary(nilCtx, record); err == nil {
		t.Errorf("expected nil context rejection")
	}
	if err := adapter.UpsertSummary(t.Context(), &contextpkg.SummaryRecord{ID: "x", SessionID: "sess-1"}); err == nil {
		t.Errorf("expected anchor turn rejection")
	}
}

type fakeContextLLM struct {
	mu    sync.Mutex
	texts []string
	err   error
}

func (f *fakeContextLLM) Name() string { return "fake-compression" }

func (f *fakeContextLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if f.err != nil {
			yield(nil, f.err)
			return
		}
		f.mu.Lock()
		texts := f.texts
		f.mu.Unlock()
		for _, text := range texts {
			if !yield(&adkmodel.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{Text: text}}}, TurnComplete: true}, nil) {
				return
			}
		}
	}
}

func TestRouterSummarizerCollectsText(t *testing.T) {
	llm := &fakeContextLLM{texts: []string{"part one ", "part two"}}
	summarizer := newRouterSummarizer(llm, contextpkg.Producer{Provider: "p", Model: "m"}, 2048)
	result, err := summarizer.Summarize(t.Context(), &contextpkg.SummarizeRequest{Task: "compression", Prompt: "summarize", Sources: []string{"a", "b"}, MaxOutputTokens: 512})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if result.Text != "part one part two" || result.Producer.Model != "m" {
		t.Errorf("result = %+v", result)
	}
}

func TestRouterSummarizerRejects(t *testing.T) {
	good := &fakeContextLLM{texts: []string{"ok"}}
	summarizer := newRouterSummarizer(good, contextpkg.Producer{Provider: "p", Model: "m"}, 2048)
	var nilCtx context.Context
	if _, err := summarizer.Summarize(nilCtx, &contextpkg.SummarizeRequest{}); err == nil {
		t.Errorf("expected nil context rejection")
	}
	if _, err := summarizer.Summarize(t.Context(), nil); err == nil {
		t.Errorf("expected nil request rejection")
	}
	if _, err := newRouterSummarizer(nil, contextpkg.Producer{}, 2048).Summarize(t.Context(), &contextpkg.SummarizeRequest{MaxOutputTokens: 1}); err == nil {
		t.Errorf("expected missing model rejection")
	}
	big := &fakeContextLLM{texts: []string{strings.Repeat("x", 9000)}}
	if _, err := newRouterSummarizer(big, contextpkg.Producer{}, 64).Summarize(t.Context(), &contextpkg.SummarizeRequest{MaxOutputTokens: 64}); err == nil {
		t.Errorf("expected oversized rejection")
	} else if code, ok := contextpkg.CodeOf(err); !ok || code != contextpkg.ErrorCodeSummaryInvalid {
		t.Errorf("code = %v, %v (%v)", code, ok, err)
	}
	empty := &fakeContextLLM{}
	if _, err := newRouterSummarizer(empty, contextpkg.Producer{}, 2048).Summarize(t.Context(), &contextpkg.SummarizeRequest{MaxOutputTokens: 64}); err == nil {
		t.Errorf("expected empty rejection")
	} else if code, ok := contextpkg.CodeOf(err); !ok || code != contextpkg.ErrorCodeSummaryUnavailable {
		t.Errorf("code = %v, %v (%v)", code, ok, err)
	}
}

func TestCompressionProducerResolves(t *testing.T) {
	cfgPath, _ := writeContextCLIConfig(t, "")
	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if _, err := compressionProducer(nil); err == nil {
		t.Errorf("expected nil config rejection")
	}
	if _, err := compressionProducer(loaded.Config); err == nil {
		t.Errorf("expected missing route rejection")
	}
	routedDir := t.TempDir()
	routed := routedDir + "/routed.yaml"
	content := `version: 1
models:
  definitions:
    primary:
      protocol: openai_chat_compat
      model: test-model
      api_key_env: AURA_TEST_MODEL_KEY
      capabilities:
        streaming: true
        tools: true
        context_tokens: 200000
        tokenizer: test
  routing:
    compression: primary
model_routes:
  primary:
    candidates: [primary]
storage:
  path: ` + routedDir + `
tools:
  workspace: ` + routedDir + `
skills:
  roots: [` + routedDir + `]
context:
  recent_complete_turns: 5
`
	if err := os.WriteFile(routed, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	loadedRouted, err := config.Load(routed)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	producer, err := compressionProducer(loadedRouted.Config)
	if err != nil {
		t.Fatalf("compressionProducer: %v", err)
	}
	if producer.Model != "test-model" || producer.Provider == "" {
		t.Errorf("producer = %+v", producer)
	}
}
