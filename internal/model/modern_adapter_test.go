package model

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/anggasct/aura/internal/config"
)

func TestOpenAIResponsesAdapter(t *testing.T) {
	var gotAuthorization string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path = %q, want /v1/responses", r.URL.Path)
		}
		gotAuthorization = r.Header.Get("Authorization")
		writeFixture(t, w, fixtureBytes(t, "openai_responses.json"))
	}))
	defer srv.Close()

	adapter, err := NewOpenAIResponsesAdapter("gpt-5", srv.URL, "response-key", 0)
	if err != nil {
		t.Fatalf("NewOpenAIResponsesAdapter: %v", err)
	}
	responses, err := collect(adapter.GenerateContent(context.Background(), sampleRequest("hello"), false))
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	if gotAuthorization != "Bearer response-key" {
		t.Errorf("Authorization = %q", gotAuthorization)
	}
	if len(responses) != 1 || responses[0].Content.Parts[0].Text != "Hello from Responses." {
		t.Fatalf("responses = %+v", responses)
	}
	if responses[0].UsageMetadata.TotalTokenCount != 13 {
		t.Errorf("usage = %+v", responses[0].UsageMetadata)
	}
}

func TestGeminiAdapter(t *testing.T) {
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ":generateContent") {
			t.Errorf("path = %q, want generateContent endpoint", r.URL.Path)
		}
		gotKey = r.Header.Get("x-goog-api-key")
		writeFixture(t, w, fixtureBytes(t, "gemini_response.json"))
	}))
	defer srv.Close()

	adapter, err := NewGeminiAdapter("gemini-2.5-pro", srv.URL, "gemini-key", 0)
	if err != nil {
		t.Fatalf("NewGeminiAdapter: %v", err)
	}
	responses, err := collect(adapter.GenerateContent(context.Background(), sampleRequest("hello"), false))
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	if gotKey != "gemini-key" {
		t.Errorf("x-goog-api-key = %q", gotKey)
	}
	if len(responses) != 1 || responses[0].Content.Parts[0].Text != "Hello from Gemini." {
		t.Fatalf("responses = %+v", responses)
	}
}

func TestGeminiStreamFixture(t *testing.T) {
	fixture := fixtureBytes(t, "gemini_stream.txt")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeFixture(t, w, fixture)
	}))
	defer srv.Close()

	adapter, err := NewGeminiAdapter("gemini-2.5-pro", srv.URL, "gemini-key", 0)
	if err != nil {
		t.Fatalf("NewGeminiAdapter: %v", err)
	}
	responses, err := collect(adapter.GenerateContent(context.Background(), sampleRequest("hello"), true))
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	var partials []string
	var final *adkmodel.LLMResponse
	for _, r := range responses {
		if r.Partial {
			partials = append(partials, r.Content.Parts[0].Text)
			continue
		}
		final = r
	}
	if !slices.Equal(partials, []string{"Hello", " world"}) {
		t.Fatalf("partials = %v", partials)
	}
	if final == nil || !final.TurnComplete {
		t.Fatalf("final = %+v, want a complete terminal response", final)
	}
	if got := final.Content.Parts[0].Text; got != " world" {
		t.Errorf("final text = %q, want %q", got, " world")
	}
	if final.UsageMetadata == nil || final.UsageMetadata.TotalTokenCount != 8 {
		t.Errorf("final usage = %+v", final.UsageMetadata)
	}
}

func TestGeminiStreamTruncatedBeforeTerminal(t *testing.T) {
	fixture := fixtureBytes(t, "gemini_stream.txt")
	// Cut at the event boundary before the STOP event so the stream is
	// complete events that never reach the terminal marker.
	stopAt := bytes.Index(fixture, []byte(`"finishReason":"STOP"`))
	eventStart := bytes.LastIndex(fixture[:stopAt], []byte("\n\n"))
	truncated := fixture[:eventStart+2]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(truncated)
	}))
	defer srv.Close()

	adapter, err := NewGeminiAdapter("gemini-2.5-pro", srv.URL, "gemini-key", 0)
	if err != nil {
		t.Fatalf("NewGeminiAdapter: %v", err)
	}
	responses, err := collect(adapter.GenerateContent(context.Background(), sampleRequest("hello"), true))
	if err == nil {
		t.Fatal("expected truncation error, got nil")
	}
	wantCode(t, err, ErrorCodeStreamInvalid)
	if len(responses) == 0 {
		t.Error("partials should still be delivered before the truncation error")
	}
}

func TestModernProtocolBuildRouter(t *testing.T) {
	t.Setenv("TEST_MODEL_API_KEY", "key")
	router, err := BuildRouter(nil, config.Models{
		Definitions: map[string]config.ModelDefinition{
			"primary":   configuredDefinition(config.ProtocolOpenAIResponses, "gpt-5", "https://api.openai.com"),
			"auxiliary": configuredDefinition(config.ProtocolGeminiNative, "gemini-2.5-pro", "https://generativelanguage.googleapis.com"),
		},
	})
	if err != nil {
		t.Fatalf("BuildRouter: %v", err)
	}
	primary, err := router.For("turn")
	if err != nil {
		t.Fatalf("For(turn): %v", err)
	}
	if _, ok := primary.(*OpenAIResponsesAdapter); !ok {
		t.Fatalf("primary = %T, want *OpenAIResponsesAdapter", primary)
	}
	auxiliary, err := router.For("summarize")
	if err != nil {
		t.Fatalf("For(summarize): %v", err)
	}
	if _, ok := auxiliary.(*GeminiAdapter); !ok {
		t.Fatalf("auxiliary = %T, want *GeminiAdapter", auxiliary)
	}
}

func TestBuildRouterRejectsTaskWithoutCapability(t *testing.T) {
	t.Setenv("TEST_MODEL_API_KEY", "key")
	_, err := BuildRouter(nil, config.Models{
		Definitions: map[string]config.ModelDefinition{
			"primary": configuredDefinition(config.ProtocolOpenAIResponses, "gpt-5", "https://api.openai.com"),
		},
		Routing: map[string]string{"vision": "primary"},
	})
	if code, ok := CodeOf(err); !ok || code != ErrorCodeCapabilityUnsupported {
		t.Fatalf("CodeOf(%v) = %q, %v; want %q, true", err, code, ok, ErrorCodeCapabilityUnsupported)
	}
}

func TestModelTelemetryContainsSafeRequestMetadata(t *testing.T) {
	previous := slog.Default()
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeFixture(t, w, fixtureBytes(t, "openai_responses.json"))
	}))
	defer srv.Close()

	adapter, err := NewOpenAIResponsesAdapter("gpt-5", srv.URL, "never-log-key", 0)
	if err != nil {
		t.Fatalf("NewOpenAIResponsesAdapter: %v", err)
	}
	request := sampleRequest("never-log-content")
	if _, err := collect(adapter.GenerateContent(WithTask(context.Background(), "summarize"), request, false)); err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	if !strings.Contains(logs.String(), "protocol=openai_responses") {
		t.Errorf("logs missing protocol: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "model=gpt-5") || !strings.Contains(logs.String(), "task=summarize") {
		t.Errorf("logs missing model/task: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "input_tokens=") || !strings.Contains(logs.String(), "retry_count=") {
		t.Errorf("logs missing usage/retry fields: %s", logs.String())
	}
	if strings.Contains(logs.String(), "never-log-key") || strings.Contains(logs.String(), "never-log-content") {
		t.Errorf("logs leaked secret or content: %s", logs.String())
	}
}
