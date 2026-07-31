package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

var _ adkmodel.LLM = (*OpenAIAdapter)(nil)

func fixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "model", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func writeFixture(t *testing.T, w http.ResponseWriter, b []byte) {
	t.Helper()
	if _, err := w.Write(b); err != nil {
		t.Fatalf("write response: %v", err)
	}
}

func collect(seq iter.Seq2[*adkmodel.LLMResponse, error]) ([]*adkmodel.LLMResponse, error) {
	var resps []*adkmodel.LLMResponse
	for r, err := range seq {
		if err != nil {
			return resps, err
		}
		resps = append(resps, r)
	}
	return resps, nil
}

func sampleRequest(text string) *adkmodel.LLMRequest {
	return &adkmodel.LLMRequest{
		Model: "gpt-4o",
		Contents: []*genai.Content{{
			Role:  "user",
			Parts: []*genai.Part{{Text: text}},
		}},
	}
}

func mustOpenAIAdapter(t *testing.T, name, baseURL, apiKey string, timeout time.Duration) *OpenAIAdapter {
	t.Helper()
	a, err := NewOpenAIAdapter(name, baseURL, apiKey, timeout)
	if err != nil {
		t.Fatalf("NewOpenAIAdapter: %v", err)
	}
	return a
}

func TestOpenAI_NonStreaming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeFixture(t, w, fixtureBytes(t, "openai_completion.json"))
	}))
	defer srv.Close()

	a := mustOpenAIAdapter(t, "gpt-4o", srv.URL, "sk-test", 0)
	resps, err := collect(a.GenerateContent(context.Background(), sampleRequest("hi"), false))
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	if len(resps) != 1 {
		t.Fatalf("got %d responses, want 1", len(resps))
	}
	r := resps[0]
	if got := r.Content.Parts[0].Text; got != "Hello from the model." {
		t.Errorf("text = %q", got)
	}
	if r.UsageMetadata.PromptTokenCount != 12 || r.UsageMetadata.CandidatesTokenCount != 5 {
		t.Errorf("usage = %+v", r.UsageMetadata)
	}
	if !r.TurnComplete {
		t.Error("TurnComplete = false, want true")
	}
}

func TestOpenAI_ToolCallResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeFixture(t, w, fixtureBytes(t, "tool_call_openai.json"))
	}))
	defer srv.Close()

	a := mustOpenAIAdapter(t, "gpt-4o", srv.URL, "sk-test", 0)
	resps, err := collect(a.GenerateContent(context.Background(), sampleRequest("search"), false))
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	fc := resps[0].Content.Parts[0].FunctionCall
	if fc == nil {
		t.Fatal("expected a FunctionCall part")
	}
	if fc.Name != "search" || fc.ID != "call_abc" {
		t.Errorf("function call = %+v", fc)
	}
	if fc.Args["query"] != "go iter.Seq2" {
		t.Errorf("args = %+v", fc.Args)
	}
}

func TestOpenAI_Streaming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeFixture(t, w, fixtureBytes(t, "openai_stream.txt"))
	}))
	defer srv.Close()

	a := mustOpenAIAdapter(t, "gpt-4o", srv.URL, "sk-test", 0)
	resps, err := collect(a.GenerateContent(context.Background(), sampleRequest("hi"), true))
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	var text string
	var final *adkmodel.LLMResponse
	for _, r := range resps {
		for _, p := range r.Content.Parts {
			if p.Text != "" {
				text += p.Text
			}
		}
		if r.TurnComplete {
			final = r
		}
	}
	if text != "Hello world" {
		t.Errorf("streamed text = %q, want %q", text, "Hello world")
	}
	if final == nil || final.UsageMetadata.TotalTokenCount != 5 {
		t.Errorf("final usage missing/wrong: %+v", final)
	}
}

func TestOpenAI_AuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	a := mustOpenAIAdapter(t, "gpt-4o", srv.URL, "bad-key", 0)
	_, err := collect(a.GenerateContent(context.Background(), sampleRequest("hi"), false))
	if !errors.Is(err, ErrAuthFailed) {
		t.Errorf("err = %v, want ErrAuthFailed", err)
	}
}

func TestOpenAI_RateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	a := mustOpenAIAdapter(t, "gpt-4o", srv.URL, "sk-test", 0)
	a.retry.MaxRetries = 0
	_, err := collect(a.GenerateContent(context.Background(), sampleRequest("hi"), false))
	if !errors.Is(err, ErrRateLimited) {
		t.Errorf("err = %v, want ErrRateLimited", err)
	}
}

func TestOpenAI_BaseURLAndAuth(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		writeFixture(t, w, fixtureBytes(t, "openai_completion.json"))
	}))
	defer srv.Close()

	a := mustOpenAIAdapter(t, "gpt-4o", srv.URL, "sk-test", 0)
	if _, err := collect(a.GenerateContent(context.Background(), sampleRequest("hi"), false)); err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("auth = %q", gotAuth)
	}
}

func TestOpenAI_EmptyKeyOmitsAuth(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		writeFixture(t, w, fixtureBytes(t, "openai_completion.json"))
	}))
	defer srv.Close()

	a := mustOpenAIAdapter(t, "llama3", srv.URL, "", 0)
	if _, err := collect(a.GenerateContent(context.Background(), sampleRequest("hi"), false)); err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("empty key should omit Authorization, got %q", gotAuth)
	}
}

func TestOpenAI_StreamingCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		close(started)
		flusher.Flush()
		<-release
	}))
	defer srv.Close()
	defer close(release)

	a := mustOpenAIAdapter(t, "gpt-4o", srv.URL, "sk-test", 0)
	ctx, cancel := context.WithCancel(context.Background())
	seq := a.GenerateContent(ctx, sampleRequest("hi"), true)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range seq {
		}
	}()
	<-started
	cancel()

	start := time.Now()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not abort after context cancellation")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("stream abort took %v, want < 1s", elapsed)
	}
}

func TestOpenAI_StreamingIdleTimeout(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()

	a := mustOpenAIAdapter(t, "gpt-4o", srv.URL, "sk-test", 0)
	a.streamingIdleTimeout = 25 * time.Millisecond
	seq := a.GenerateContent(context.Background(), sampleRequest("hi"), true)
	errCh := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, err := range seq {
			if err != nil {
				errCh <- err
			}
		}
	}()
	<-started

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrStreamIdle) {
			t.Fatalf("err = %v, want ErrStreamIdle", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stream did not abort after idle timeout")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream iterator did not close")
	}
}

func TestOpenAI_StreamingInvalidToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"search\",\"arguments\":\"not-json\"}}]},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	a := mustOpenAIAdapter(t, "gpt-4o", srv.URL, "sk-test", 0)
	_, err := collect(a.GenerateContent(context.Background(), sampleRequest("search"), true))
	if !errors.Is(err, ErrInvalidToolCall) {
		t.Fatalf("err = %v, want ErrInvalidToolCall", err)
	}
}

func TestOpenAI_ToolCallArgumentsMustBeObject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"model":"gpt-4o","choices":[{"message":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"search","arguments":"[]"}}]},"finish_reason":"tool_calls"}]}`)
	}))
	defer srv.Close()

	a := mustOpenAIAdapter(t, "gpt-4o", srv.URL, "sk-test", 0)
	_, err := collect(a.GenerateContent(context.Background(), sampleRequest("search"), false))
	if !errors.Is(err, ErrInvalidToolCall) {
		t.Fatalf("err = %v, want ErrInvalidToolCall", err)
	}
}

func TestOpenAI_RequestSerializesToolCall(t *testing.T) {
	var captured openaiRequestBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		writeFixture(t, w, fixtureBytes(t, "openai_completion.json"))
	}))
	defer srv.Close()

	req := &adkmodel.LLMRequest{
		Model: "gpt-4o",
		Contents: []*genai.Content{
			{Role: "user", Parts: []*genai.Part{{Text: "find go docs"}}},
			{Role: "model", Parts: []*genai.Part{{
				FunctionCall: &genai.FunctionCall{ID: "call_1", Name: "search", Args: map[string]any{"q": "go"}},
			}}},
			{Role: "user", Parts: []*genai.Part{{
				FunctionResponse: &genai.FunctionResponse{ID: "call_1", Name: "search", Response: map[string]any{"hits": 3}},
			}}},
		},
	}
	a := mustOpenAIAdapter(t, "gpt-4o", srv.URL, "sk-test", 0)
	if _, err := collect(a.GenerateContent(context.Background(), req, false)); err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}

	var assistantMsg, toolMsg *openaiMessage
	for i := range captured.Messages {
		m := &captured.Messages[i]
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			assistantMsg = m
		}
		if m.Role == "tool" {
			toolMsg = m
		}
	}
	if assistantMsg == nil || assistantMsg.ToolCalls[0].ID != "call_1" || assistantMsg.ToolCalls[0].Function.Name != "search" {
		t.Errorf("assistant tool_calls not serialized: %+v", assistantMsg)
	}
	if toolMsg == nil || toolMsg.ToolCallID != "call_1" {
		t.Errorf("tool result message not serialized: %+v", toolMsg)
	}
}
