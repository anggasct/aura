package model

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

func TestModernProtocolBuildRouter(t *testing.T) {
	t.Setenv("TEST_MODEL_API_KEY", "key")
	router, err := BuildRouter(config.Models{
		Definitions: map[string]config.ModelDefinition{
			"primary":   configuredDefinition(config.ProtocolOpenAIResponses, "gpt-5", "https://api.openai.com"),
			"auxiliary": configuredDefinition(config.ProtocolGeminiNative, "gemini-2.5-pro", "https://generativelanguage.googleapis.com"),
		},
	})
	if err != nil {
		t.Fatalf("BuildRouter: %v", err)
	}
	if _, ok := router.For("turn").(*OpenAIResponsesAdapter); !ok {
		t.Fatalf("primary = %T, want *OpenAIResponsesAdapter", router.For("turn"))
	}
	if _, ok := router.For("summarize").(*GeminiAdapter); !ok {
		t.Fatalf("auxiliary = %T, want *GeminiAdapter", router.For("summarize"))
	}
}

func TestBuildRouterRejectsTaskWithoutCapability(t *testing.T) {
	t.Setenv("TEST_MODEL_API_KEY", "key")
	_, err := BuildRouter(config.Models{
		Definitions: map[string]config.ModelDefinition{
			"primary": configuredDefinition(config.ProtocolOpenAIResponses, "gpt-5", "https://api.openai.com"),
		},
		Routing: map[string]string{"vision": "primary"},
	})
	if code, ok := CodeOf(err); !ok || code != ErrorCodeCapabilityUnsupported {
		t.Fatalf("CodeOf(%v) = %q, %v; want %q, true", err, code, ok, ErrorCodeCapabilityUnsupported)
	}
}
