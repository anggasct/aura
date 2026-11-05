package model

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

type finishCase struct {
	name       string
	stream     bool
	body       string
	build      func(url string) adkmodel.LLM
	wantFinish genai.FinishReason
	wantText   string
	wantTokens int32
}

func sseBody(events ...string) string {
	var b strings.Builder
	for _, event := range events {
		b.WriteString("data: " + event + "\n\n")
	}
	return b.String()
}

func finishAdapters() map[string]func(url string) adkmodel.LLM {
	return map[string]func(url string) adkmodel.LLM{
		"anthropic": func(url string) adkmodel.LLM {
			return newAnthropicAdapter(nil, "claude", url, "key", 0, 0)
		},
		"openai": func(url string) adkmodel.LLM {
			return newOpenAIAdapter(nil, "gpt", url, "key", 0, 0)
		},
		"responses": func(url string) adkmodel.LLM {
			return newOpenAIResponsesAdapter(nil, "gpt", url, "key", 0, 0)
		},
		"gemini": func(url string) adkmodel.LLM {
			return newGeminiAdapter(nil, "gemini", url, "key", 0, 0)
		},
	}
}

func TestAdaptersReportFinishReason(t *testing.T) {
	adapters := finishAdapters()
	tests := []finishCase{
		{
			name:       "anthropic non-stream max tokens",
			build:      adapters["anthropic"],
			body:       `{"model":"m","content":[{"type":"text","text":"cut"}],"stop_reason":"max_tokens","usage":{"input_tokens":2,"output_tokens":3}}`,
			wantFinish: genai.FinishReasonMaxTokens,
			wantText:   "cut",
			wantTokens: 5,
		},
		{
			name:       "anthropic non-stream end turn",
			build:      adapters["anthropic"],
			body:       `{"model":"m","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}`,
			wantFinish: genai.FinishReasonStop,
			wantText:   "done",
			wantTokens: 5,
		},
		{
			name:   "anthropic stream max tokens",
			stream: true,
			build:  adapters["anthropic"],
			body: sseBody(
				`{"type":"message_start","message":{"model":"m","usage":{"input_tokens":2}}}`,
				`{"type":"content_block_delta","delta":{"type":"text_delta","text":"cu"}}`,
				`{"type":"content_block_delta","delta":{"type":"text_delta","text":"t"}}`,
				`{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":3}}`,
				`{"type":"message_stop"}`,
			),
			wantFinish: genai.FinishReasonMaxTokens,
			wantText:   "cut",
			wantTokens: 5,
		},
		{
			name:       "openai non-stream length",
			build:      adapters["openai"],
			body:       `{"model":"m","choices":[{"message":{"role":"assistant","content":"cut"},"finish_reason":"length"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`,
			wantFinish: genai.FinishReasonMaxTokens,
			wantText:   "cut",
			wantTokens: 5,
		},
		{
			name:       "openai non-stream stop",
			build:      adapters["openai"],
			body:       `{"model":"m","choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`,
			wantFinish: genai.FinishReasonStop,
			wantText:   "done",
			wantTokens: 5,
		},
		{
			name:   "openai stream length with trailing usage chunk",
			stream: true,
			build:  adapters["openai"],
			body: sseBody(
				`{"choices":[{"delta":{"role":"assistant","content":"cu"}}]}`,
				`{"choices":[{"delta":{"content":"t"}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"length"}]}`,
				`{"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`,
				`[DONE]`,
			),
			wantFinish: genai.FinishReasonMaxTokens,
			wantText:   "cut",
			wantTokens: 5,
		},
		{
			name:       "responses non-stream incomplete",
			build:      adapters["responses"],
			body:       `{"model":"m","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","content":[{"type":"output_text","text":"cut"}]}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`,
			wantFinish: genai.FinishReasonMaxTokens,
			wantText:   "cut",
			wantTokens: 5,
		},
		{
			name:   "responses stream incomplete",
			stream: true,
			build:  adapters["responses"],
			body: sseBody(
				`{"type":"response.output_text.delta","delta":"cu"}`,
				`{"type":"response.output_text.delta","delta":"t"}`,
				`{"type":"response.incomplete","response":{"model":"m","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","content":[{"type":"output_text","text":"cut"}]}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}`,
			),
			wantFinish: genai.FinishReasonMaxTokens,
			wantText:   "cut",
			wantTokens: 5,
		},
		{
			name:   "responses stream completed",
			stream: true,
			build:  adapters["responses"],
			body: sseBody(
				`{"type":"response.completed","response":{"model":"m","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}`,
			),
			wantFinish: genai.FinishReasonStop,
			wantText:   "done",
			wantTokens: 5,
		},
		{
			name:       "gemini non-stream max tokens",
			build:      adapters["gemini"],
			body:       `{"candidates":[{"content":{"parts":[{"text":"cut"}]},"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"totalTokenCount":5}}`,
			wantFinish: genai.FinishReasonMaxTokens,
			wantText:   "cut",
			wantTokens: 5,
		},
		{
			name:   "gemini stream max tokens joins every chunk",
			stream: true,
			build:  adapters["gemini"],
			body: sseBody(
				`{"candidates":[{"content":{"parts":[{"text":"cu"}]}}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}`,
				`{"candidates":[{"content":{"parts":[{"text":"t"}]},"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"totalTokenCount":5}}`,
			),
			wantFinish: genai.FinishReasonMaxTokens,
			wantText:   "cut",
			wantTokens: 5,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			contentType := "application/json"
			if tc.stream {
				contentType = "text/event-stream"
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", contentType)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			responses, err := collect(tc.build(srv.URL).GenerateContent(context.Background(), sampleRequest("hi"), tc.stream))
			if err != nil {
				t.Fatalf("GenerateContent: %v", err)
			}
			final := responses[len(responses)-1]
			if final.Partial {
				t.Fatalf("last response is partial: %+v", final)
			}
			if final.FinishReason != tc.wantFinish {
				t.Errorf("finish reason = %q, want %q", final.FinishReason, tc.wantFinish)
			}
			if got := final.Content.Parts[0].Text; got != tc.wantText {
				t.Errorf("final text = %q, want %q", got, tc.wantText)
			}
			if final.UsageMetadata == nil || final.UsageMetadata.TotalTokenCount != tc.wantTokens {
				t.Errorf("final usage = %+v, want %d total tokens", final.UsageMetadata, tc.wantTokens)
			}
			for _, response := range responses[:len(responses)-1] {
				if !response.Partial || response.UsageMetadata != nil {
					t.Errorf("intermediate response = %+v, want a partial without usage", response)
				}
			}
		})
	}
}

func TestOpenAIStreamRequestAsksForUsage(t *testing.T) {
	tests := []struct {
		name      string
		stream    bool
		wantUsage bool
	}{
		{name: "stream", stream: true, wantUsage: true},
		{name: "non stream", stream: false, wantUsage: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body, err := buildOpenAIRequest(sampleRequest("hi"), tc.stream)
			if err != nil {
				t.Fatalf("buildOpenAIRequest: %v", err)
			}
			var decoded struct {
				StreamOptions *struct {
					IncludeUsage bool `json:"include_usage"`
				} `json:"stream_options"`
			}
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			if got := decoded.StreamOptions != nil && decoded.StreamOptions.IncludeUsage; got != tc.wantUsage {
				t.Errorf("include_usage = %v, want %v", got, tc.wantUsage)
			}
		})
	}
}
