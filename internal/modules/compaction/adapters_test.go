package compaction

import (
	"context"
	"database/sql"
	"encoding/json"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

func contextRow(kind, payload string) *turn.Event {
	return &turn.Event{
		ID: "evt-1", SessionID: "sess-1", Sequence: 4, TurnID: "t1",
		InvocationID: "inv-1", Author: "owner", Kind: kind, SchemaVersion: 1,
		Payload: json.RawMessage(payload), CreatedAt: time.Now().UTC(),
	}
}

func TestMapContextEventText(t *testing.T) {
	row := contextRow("message.completed", `{"content":{"parts":[{"text":"hello "},{"text":"world"}]}}`)
	event := MapEvent(row)
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
			if got := MapEvent(contextRow(tc.kind, tc.payload)); got.Text != tc.text {
				t.Errorf("text = %q", got.Text)
			}
		})
	}
}

func TestMapContextEventCorrelation(t *testing.T) {
	row := contextRow("tool.requested", `{"tool_call_id":"call-1","effect_intent_id":"intent-1","approval_id":"appr-1"}`)
	if got := MapEvent(row); got.CorrelationID != "call-1" {
		t.Errorf("correlation = %q", got.CorrelationID)
	}
	row = contextRow("channel.requested", `{"effect_intent_id":"intent-2"}`)
	if got := MapEvent(row); got.CorrelationID != "intent-2" {
		t.Errorf("correlation = %q", got.CorrelationID)
	}
	row = contextRow("approval.required", `{"approval_id":"appr-3"}`)
	if got := MapEvent(row); got.CorrelationID != "appr-3" {
		t.Errorf("correlation = %q", got.CorrelationID)
	}
}

func TestMapContextEventToolResults(t *testing.T) {
	row := contextRow("tool.completed", `{"text":"output"}`)
	if got := MapEvent(row); !got.ToolResult {
		t.Errorf("tool.completed not flagged: %+v", got)
	}
	row = contextRow("adk_event", `{"content":{"parts":[{"functionCall":{"id":"call-9","name":"run","args":{"cmd":"ls"}}}]}}`)
	callEvt := MapEvent(row)
	if callEvt.ToolResult || callEvt.CorrelationID != "" || !strings.Contains(callEvt.Text, "run") || !strings.Contains(callEvt.Text, `"cmd":"ls"`) {
		t.Errorf("function call not mapped: %+v", callEvt)
	}
	row = contextRow("adk_event", `{"content":{"parts":[{"functionResponse":{"id":"call-9","name":"run","response":{"output":"listed files"}}}]}}`)
	got := MapEvent(row)
	if !got.ToolResult || got.CorrelationID != "" || !strings.Contains(got.Text, "listed files") {
		t.Errorf("function response not detected: %+v", got)
	}
	row = contextRow("message.completed", `{"text":"plain"}`)
	if got := MapEvent(row); got.ToolResult {
		t.Errorf("plain message flagged: %+v", got)
	}
}

func TestMapContextEventMediaType(t *testing.T) {
	row := contextRow("adk_event", `{"content":{"parts":[{"inlineData":{"mimeType":"image/png"}}]}}`)
	if got := MapEvent(row); got.MediaType != "image/png" {
		t.Errorf("media = %q", got.MediaType)
	}
	row = contextRow("adk_event", `{"content":{"parts":[{"fileData":{"mimeType":"audio/ogg"}}]}}`)
	if got := MapEvent(row); got.MediaType != "audio/ogg" {
		t.Errorf("media = %q", got.MediaType)
	}
}

func seedContextSession(t *testing.T, db *sql.DB) {
	t.Helper()
	sessions := turnstore.NewSessionService(db)
	if err := sessions.Create(t.Context(), &turn.Session{ID: "sess-1", OwnerID: "owner-1", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), Metadata: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	events := turnstore.NewEventStore(db)
	for i, text := range []string{"first", "second"} {
		if _, err := events.AppendSequenced(t.Context(), "sess-1", &turn.Event{
			ID: "evt-" + string(rune('a'+i)), SessionID: "sess-1", TurnID: "t1",
			InvocationID: "inv-old", Author: "owner", Kind: "message.completed",
			SchemaVersion: 1, Payload: json.RawMessage(`{"text":"` + text + `"}`), CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
}

func TestContextSummaryStoreRoundTrip(t *testing.T) {
	db := sqlitetest.OpenMigrated(t, turnstore.Schema())
	seedContextSession(t, db)
	adapter, err := NewEventSummaryStore(turnstore.NewSessionService(db), turnstore.NewEventStore(db))
	if err != nil {
		t.Fatalf("NewEventSummaryStore: %v", err)
	}
	listed, err := adapter.ListSummaries(t.Context(), "sess-1")
	if err != nil {
		t.Fatalf("ListSummaries: %v", err)
	}
	if len(listed) != 0 {
		t.Errorf("listed = %+v", listed)
	}
	payload := []byte(`{"source":{"start_sequence":1,"end_sequence":2,"event_ids":[],"digest":"abc"},"producer":{"provider":"p","model":"m","model_version":"","prompt_version":"v1","prompt_digest":"d"},"tokens":{"source":4,"summary":2,"accounting":"exact"},"trust":"derived_untrusted","summary":{"goals":["g"],"decisions":[],"constraints":[],"open_work":[],"facts":[]},"generated_at":"2026-09-14T00:00:00Z","kind":"context.summary.v1","schema_version":1}`)
	record := &SummaryRecord{ID: "ctxsum-test", SessionID: "sess-1", StartSequence: 1, EndSequence: 2, TurnID: "t1", Payload: payload, CreatedAt: time.Now().UTC()}
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
	if err := adapter.UpsertSummary(t.Context(), &SummaryRecord{ID: "x", SessionID: "sess-1"}); err == nil {
		t.Errorf("expected anchor turn rejection")
	}
}

func TestEventSummaryStoreRefusesNilPorts(t *testing.T) {
	db := sqlitetest.OpenMigrated(t, turnstore.Schema())
	sessions := turnstore.NewSessionService(db)
	events := turnstore.NewEventStore(db)
	if _, err := NewEventSummaryStore(nil, events); err == nil {
		t.Errorf("expected nil lister rejection")
	} else if !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Errorf("err = %v, want %s", err, ErrorCodeInvalidArgument)
	}
	if _, err := NewEventSummaryStore(sessions, nil); err == nil {
		t.Errorf("expected nil upserter rejection")
	} else if !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Errorf("err = %v, want %s", err, ErrorCodeInvalidArgument)
	}
}

type fakeLLM struct {
	mu    sync.Mutex
	texts []string
	err   error
}

func (f *fakeLLM) Name() string { return "fake-compression" }

func (f *fakeLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
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
	llm := &fakeLLM{texts: []string{"part one ", "part two"}}
	summarizer, err := NewLLMSummarizer(llm, Producer{Provider: "p", Model: "m"}, 2048)
	if err != nil {
		t.Fatalf("NewLLMSummarizer: %v", err)
	}
	result, err := summarizer.Summarize(t.Context(), &SummarizeRequest{Task: "compression", Prompt: "summarize", Sources: []string{"a", "b"}, MaxOutputTokens: 512})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if result.Text != "part one part two" || result.Producer.Model != "m" {
		t.Errorf("result = %+v", result)
	}
}

func TestRouterSummarizerRejects(t *testing.T) {
	good := &fakeLLM{texts: []string{"ok"}}
	summarizer, err := NewLLMSummarizer(good, Producer{Provider: "p", Model: "m"}, 2048)
	if err != nil {
		t.Fatalf("NewLLMSummarizer: %v", err)
	}
	var nilCtx context.Context
	if _, err := summarizer.Summarize(nilCtx, &SummarizeRequest{}); err == nil {
		t.Errorf("expected nil context rejection")
	}
	if _, err := summarizer.Summarize(t.Context(), nil); err == nil {
		t.Errorf("expected nil request rejection")
	}
	if _, err := (&LLMSummarizer{bound: OutputByteBound(2048) + 1}).Summarize(t.Context(), &SummarizeRequest{MaxOutputTokens: 1}); err == nil {
		t.Errorf("expected missing model rejection")
	}
	big := &fakeLLM{texts: []string{strings.Repeat("x", 9000)}}
	bigSummarizer, err := NewLLMSummarizer(big, Producer{}, 64)
	if err != nil {
		t.Fatalf("NewLLMSummarizer: %v", err)
	}
	if _, err := bigSummarizer.Summarize(t.Context(), &SummarizeRequest{MaxOutputTokens: 64}); err == nil {
		t.Errorf("expected oversized rejection")
	} else if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrorCodeSummaryInvalid {
		t.Errorf("code = %v, %v (%v)", code, ok, err)
	}
	empty := &fakeLLM{}
	emptySummarizer, err := NewLLMSummarizer(empty, Producer{}, 2048)
	if err != nil {
		t.Fatalf("NewLLMSummarizer: %v", err)
	}
	if _, err := emptySummarizer.Summarize(t.Context(), &SummarizeRequest{MaxOutputTokens: 64}); err == nil {
		t.Errorf("expected empty rejection")
	} else if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrorCodeSummaryUnavailable {
		t.Errorf("code = %v, %v (%v)", code, ok, err)
	}
}

func TestLLMSummarizerRefusesInvalidArgs(t *testing.T) {
	llm := &fakeLLM{texts: []string{"ok"}}
	if _, err := NewLLMSummarizer(nil, Producer{Provider: "p", Model: "m"}, 2048); err == nil {
		t.Errorf("expected nil llm rejection")
	} else if !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Errorf("err = %v, want %s", err, ErrorCodeInvalidArgument)
	}
	for name, maxTokens := range map[string]int{
		"zero":      0,
		"negative":  -1,
		"oversized": maxSummarizerOutputTokens + 1,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewLLMSummarizer(llm, Producer{Provider: "p", Model: "m"}, maxTokens); err == nil {
				t.Errorf("expected invalid output bound rejection")
			} else if !fault.HasCode(err, ErrorCodeInvalidArgument) {
				t.Errorf("err = %v, want %s", err, ErrorCodeInvalidArgument)
			}
		})
	}
}

func TestCompressionProducerResolves(t *testing.T) {
	unrouted := model.Config{
		Definitions: map[string]model.Definition{
			"primary": {Protocol: model.ProtocolOpenAIChatCompat, Model: "test-model"},
		},
	}
	if _, err := ResolveProducer(unrouted, nil); err == nil {
		t.Errorf("expected missing route rejection")
	}
	if _, err := ResolveProducer(unrouted, model.Routes{"auxiliary": {}}); err == nil {
		t.Errorf("expected empty route candidate rejection")
	}
	if _, err := ResolveProducer(unrouted, model.Routes{"auxiliary": {Candidates: []string{"missing"}}}); err == nil {
		t.Errorf("expected missing candidate definition rejection")
	}
	routed := model.Config{
		Definitions: map[string]model.Definition{
			"primary": {Protocol: model.ProtocolOpenAIChatCompat, Model: "test-model"},
		},
		Routing: map[string]string{
			"compression": "primary",
		},
	}
	routes := model.Routes{
		"primary": {Candidates: []string{"primary"}},
	}
	producer, err := ResolveProducer(routed, routes)
	if err != nil {
		t.Fatalf("ResolveProducer: %v", err)
	}
	if producer.Model != "test-model" || producer.Provider != model.ProtocolOpenAIChatCompat {
		t.Errorf("producer = %+v", producer)
	}
}
