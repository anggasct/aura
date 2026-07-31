package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

var _ adkmodel.LLM = (*AnthropicAdapter)(nil)

func mustAnthropicAdapter(t *testing.T, name, baseURL, apiKey string, timeout time.Duration) *AnthropicAdapter {
	t.Helper()
	a, err := NewAnthropicAdapter(name, baseURL, apiKey, timeout)
	if err != nil {
		t.Fatalf("NewAnthropicAdapter: %v", err)
	}
	return a
}

func TestAnthropic_NonStreaming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeFixture(t, w, fixtureBytes(t, "anthropic_message.json"))
	}))
	defer srv.Close()

	a := mustAnthropicAdapter(t, "claude-sonnet-4-20250514", srv.URL, "sk-ant", 0)
	resps, err := collect(a.GenerateContent(context.Background(), sampleRequest("hi"), false))
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	if len(resps) != 1 {
		t.Fatalf("got %d responses, want 1", len(resps))
	}
	r := resps[0]
	if got := r.Content.Parts[0].Text; got != "Hello from Claude." {
		t.Errorf("text = %q", got)
	}
	if !r.TurnComplete {
		t.Error("TurnComplete = false")
	}
	if r.UsageMetadata.PromptTokenCount != 10 || r.UsageMetadata.CandidatesTokenCount != 4 {
		t.Errorf("usage = %+v", r.UsageMetadata)
	}
}

func TestAnthropic_ToolUseResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeFixture(t, w, fixtureBytes(t, "tool_call_anthropic.json"))
	}))
	defer srv.Close()

	a := mustAnthropicAdapter(t, "claude-sonnet-4-20250514", srv.URL, "sk-ant", 0)
	resps, err := collect(a.GenerateContent(context.Background(), sampleRequest("search go docs"), false))
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	fc := resps[0].Content.Parts[0].FunctionCall
	if fc == nil {
		t.Fatal("expected FunctionCall part")
	}
	if fc.Name != "search" || fc.ID != "toolu_001" {
		t.Errorf("function call = %+v", fc)
	}
	if fc.Args["query"] != "go iter.Seq2" {
		t.Errorf("args = %+v", fc.Args)
	}
}

func TestAnthropic_Streaming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeFixture(t, w, fixtureBytes(t, "anthropic_stream.txt"))
	}))
	defer srv.Close()

	a := mustAnthropicAdapter(t, "claude-sonnet-4-20250514", srv.URL, "sk-ant", 0)
	resps, err := collect(a.GenerateContent(context.Background(), sampleRequest("hi"), true))
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}

	var partialCount int
	var partialText string
	var final *adkmodel.LLMResponse
	for _, r := range resps {
		if r.Partial {
			partialCount++
			for _, p := range r.Content.Parts {
				if p.Text != "" {
					partialText += p.Text
				}
			}
		} else if r.TurnComplete {
			final = r
		}
	}
	if partialCount == 0 {
		t.Fatal("expected partial responses, got 0")
	}
	if final == nil {
		t.Fatal("expected final TurnComplete response")
	}
	var finalText string
	for _, p := range final.Content.Parts {
		if p.Text != "" {
			finalText += p.Text
		}
	}
	if finalText != "Hello Claude" {
		t.Errorf("final text = %q, want %q", finalText, "Hello Claude")
	}
	if partialText != "Hello Claude" {
		t.Errorf("partial text = %q, want %q", partialText, "Hello Claude")
	}
}

func TestAnthropic_AuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	a := mustAnthropicAdapter(t, "claude", srv.URL, "bad-key", 0)
	_, err := collect(a.GenerateContent(context.Background(), sampleRequest("hi"), false))
	if !errors.Is(err, ErrAuthFailed) {
		t.Errorf("err = %v, want ErrAuthFailed", err)
	}
}

func TestAnthropic_Headers(t *testing.T) {
	var gotVersion, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVersion = r.Header.Get("anthropic-version")
		gotAuth = r.Header.Get("x-api-key")
		writeFixture(t, w, fixtureBytes(t, "anthropic_message.json"))
	}))
	defer srv.Close()

	a := mustAnthropicAdapter(t, "claude-sonnet-4-20250514", srv.URL, "sk-ant", 0)
	if _, err := collect(a.GenerateContent(context.Background(), sampleRequest("hi"), false)); err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	if gotVersion != anthropicVersion {
		t.Errorf("anthropic-version = %q, want %q", gotVersion, anthropicVersion)
	}
	if gotAuth != "sk-ant" {
		t.Errorf("x-api-key = %q", gotAuth)
	}
}

func TestAnthropic_RequestBuild(t *testing.T) {
	var captured anthropicRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		writeFixture(t, w, fixtureBytes(t, "anthropic_message.json"))
	}))
	defer srv.Close()

	req := &adkmodel.LLMRequest{
		Model: "claude-sonnet-4-20250514",
		Config: &genai.GenerateContentConfig{
			SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: "you are helpful"}}},
			MaxOutputTokens:   512,
		},
		Contents: []*genai.Content{
			{Role: "user", Parts: []*genai.Part{{Text: "find go docs"}}},
			{Role: "model", Parts: []*genai.Part{{
				FunctionCall: &genai.FunctionCall{ID: "toolu_call_1", Name: "search", Args: map[string]any{"q": "go"}},
			}}},
			{Role: "user", Parts: []*genai.Part{{
				FunctionResponse: &genai.FunctionResponse{ID: "toolu_call_1", Name: "search", Response: map[string]any{"hits": 3}},
			}}},
		},
	}
	a := mustAnthropicAdapter(t, "claude-sonnet-4-20250514", srv.URL, "sk-ant", 0)
	if _, err := collect(a.GenerateContent(context.Background(), req, false)); err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	if captured.System != "you are helpful" {
		t.Errorf("system = %q", captured.System)
	}
	if captured.MaxTokens != 512 {
		t.Errorf("max_tokens = %d", captured.MaxTokens)
	}
	// find assistant message with tool_use
	var foundToolUse, foundToolResult bool
	for _, msg := range captured.Messages {
		for _, block := range msg.Content {
			if block.Type == "tool_use" && block.ID == "toolu_call_1" {
				foundToolUse = true
			}
			if block.Type == "tool_result" && block.ToolUseID == "toolu_call_1" {
				foundToolResult = true
			}
		}
	}
	if !foundToolUse {
		t.Error("tool_use block not serialized in request")
	}
	if !foundToolResult {
		t.Error("tool_result block not serialized in request")
	}
}

func TestAnthropic_ToolUseArgumentsMustBeObject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"model":"claude","content":[{"type":"tool_use","id":"toolu_1","name":"search","input":[]}] ,"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer srv.Close()

	a := mustAnthropicAdapter(t, "claude", srv.URL, "sk-ant", 0)
	_, err := collect(a.GenerateContent(context.Background(), sampleRequest("search"), false))
	if !errors.Is(err, ErrInvalidToolCall) {
		t.Fatalf("err = %v, want ErrInvalidToolCall", err)
	}
}

func TestAnthropic_StreamingInvalidToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"model\":\"claude\",\"usage\":{}}}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"search\"}}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"partial_json\":\"not-json\"}}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	a := mustAnthropicAdapter(t, "claude", srv.URL, "sk-ant", 0)
	_, err := collect(a.GenerateContent(context.Background(), sampleRequest("search"), true))
	if !errors.Is(err, ErrInvalidToolCall) {
		t.Fatalf("err = %v, want ErrInvalidToolCall", err)
	}
}

func TestAnthropic_StreamingStopsWhenCallerStops(t *testing.T) {
	serverDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, data := range []string{
			`{"type":"message_start","message":{"model":"claude","usage":{}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"text":"hello"}}`,
			`{"type":"message_stop"}`,
		} {
			_, _ = io.WriteString(w, "data: "+data+"\n\n")
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
		close(serverDone)
	}))
	defer srv.Close()

	a := mustAnthropicAdapter(t, "claude", srv.URL, "sk-ant", 0)
	for response, err := range a.GenerateContent(context.Background(), sampleRequest("hi"), true) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
		if response.TurnComplete {
			break
		}
	}
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("server did not observe iterator cancellation")
	}
}
