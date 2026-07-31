package model

import (
	"context"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/anggasct/aura/internal/config"
)

type stubLLM struct {
	name string
}

func (s *stubLLM) Name() string { return s.name }

func (s *stubLLM) GenerateContent(context.Context, *adkmodel.LLMRequest, bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {}
}

func TestRouter_ForDefaults(t *testing.T) {
	primary := &stubLLM{name: "primary"}
	auxiliary := &stubLLM{name: "auxiliary"}
	r, err := NewRouter(primary, auxiliary, nil)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	if got := r.For("turn"); got != primary {
		t.Errorf("For(turn) = %v, want primary", got)
	}
	if got := r.For(""); got != primary {
		t.Errorf("For(empty) = %v, want primary", got)
	}
	for _, task := range []string{"summarize", "title_gen", "curator", "context_compress", "profiling"} {
		if got := r.For(task); got != auxiliary {
			t.Errorf("For(%s) = %v, want auxiliary", task, got)
		}
	}
	if got := r.For("vision"); got != primary {
		t.Errorf("For(vision) = %v, want primary", got)
	}
	if got := r.For("unknown_task"); got != auxiliary {
		t.Errorf("For(unknown_task) = %v, want auxiliary", got)
	}
}

func TestRouter_RoutingOverridesMergeWithDefaults(t *testing.T) {
	primary := &stubLLM{name: "primary"}
	auxiliary := &stubLLM{name: "auxiliary"}
	r, err := NewRouter(primary, auxiliary, map[string]string{"summarize": "primary"})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	if got := r.For("summarize"); got != primary {
		t.Errorf("For(summarize) = %v, want primary (override)", got)
	}
	if got := r.For("vision"); got != primary {
		t.Errorf("For(vision) = %v, want primary (default preserved)", got)
	}
	if got := r.For("title_gen"); got != auxiliary {
		t.Errorf("For(title_gen) = %v, want auxiliary (default preserved)", got)
	}
}

func TestBuildRouter_OpenRouterAndOllamaEndpoints(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeFixture(t, w, fixtureBytes(t, "openai_completion.json"))
	}))
	defer srv.Close()

	models := config.Models{
		Primary:   config.ModelSpec{Provider: "openrouter", Model: "deepseek-r1", APIKey: "k", BaseURL: srv.URL},
		Auxiliary: config.ModelSpec{Provider: "ollama", Model: "llama3", BaseURL: "http://localhost:11434"},
	}
	r, err := BuildRouter(models)
	if err != nil {
		t.Fatalf("BuildRouter: %v", err)
	}
	resps, err := collect(r.For("turn").GenerateContent(context.Background(), sampleRequest("hi"), false))
	if err != nil {
		t.Fatalf("openrouter request: %v", err)
	}
	if len(resps) != 1 || resps[0].Content.Parts[0].Text != "Hello from the model." {
		t.Errorf("unexpected response: %+v", resps)
	}
	if _, ok := r.For("summarize").(*OpenAIAdapter); !ok {
		t.Errorf("ollama auxiliary = %T, want *OpenAIAdapter", r.For("summarize"))
	}
}

func TestRouter_UnknownTaskFails(t *testing.T) {
	_, err := NewRouter(nil, nil, map[string]string{"bogus_task": "primary"})
	if err == nil || !strings.Contains(err.Error(), "unknown task") {
		t.Errorf("err = %v, want unknown task error", err)
	}
}

func TestRouter_InvalidRoleFails(t *testing.T) {
	_, err := NewRouter(nil, nil, map[string]string{"summarize": "bogus"})
	if err == nil || !strings.Contains(err.Error(), "invalid role") {
		t.Errorf("err = %v, want invalid role error", err)
	}
}

func TestBuildRouter_ProviderMapping(t *testing.T) {
	models := config.Models{
		Primary:   config.ModelSpec{Provider: "anthropic", Model: "claude", APIKey: "k", BaseURL: "https://api.anthropic.com"},
		Auxiliary: config.ModelSpec{Provider: "openrouter", Model: "llama", APIKey: "k", BaseURL: "https://openrouter.ai/api"},
	}
	r, err := BuildRouter(models)
	if err != nil {
		t.Fatalf("BuildRouter: %v", err)
	}
	if _, ok := r.For("turn").(*AnthropicAdapter); !ok {
		t.Errorf("primary = %T, want *AnthropicAdapter", r.For("turn"))
	}
	if _, ok := r.For("summarize").(*OpenAIAdapter); !ok {
		t.Errorf("auxiliary = %T, want *OpenAIAdapter", r.For("summarize"))
	}
}

func TestBuildRouter_UnconfiguredProviderIsNil(t *testing.T) {
	r, err := BuildRouter(config.Models{})
	if err != nil {
		t.Fatalf("BuildRouter: %v", err)
	}
	if r.For("turn") != nil {
		t.Errorf("unconfigured primary should be nil, got %v", r.For("turn"))
	}
	if r.For("summarize") != nil {
		t.Errorf("unconfigured auxiliary should be nil, got %v", r.For("summarize"))
	}
}

func TestBuildRouter_UnsupportedProviderFails(t *testing.T) {
	models := config.Models{Primary: config.ModelSpec{Provider: "bogus", Model: "x", APIKey: "k"}}
	if _, err := BuildRouter(models); err == nil || !strings.Contains(err.Error(), "unsupported provider") {
		t.Errorf("err = %v, want unsupported provider error", err)
	}
}

func TestBuildRouter_MissingKeyValidation(t *testing.T) {
	models := config.Models{Primary: config.ModelSpec{Provider: "anthropic", Model: "claude"}}
	if _, err := BuildRouter(models); err == nil || !strings.Contains(err.Error(), "missing api_key") {
		t.Errorf("err = %v, want missing api_key error", err)
	}

	localhost := config.Models{Primary: config.ModelSpec{Provider: "ollama", Model: "llama3", BaseURL: "http://localhost:11434"}}
	if _, err := BuildRouter(localhost); err != nil {
		t.Errorf("localhost without key should be allowed: %v", err)
	}
}

func TestBuildRouter_InvalidBaseURLFails(t *testing.T) {
	models := config.Models{Primary: config.ModelSpec{Provider: "openai", Model: "gpt-4o", APIKey: "k", BaseURL: "://bad"}}
	if _, err := BuildRouter(models); err == nil || !strings.Contains(err.Error(), "invalid base_url") {
		t.Errorf("err = %v, want invalid base_url error", err)
	}
}

func TestBuildRouter_RemoteHTTPRequiresTLS(t *testing.T) {
	models := config.Models{Primary: config.ModelSpec{
		Provider: "openai",
		Model:    "gpt-4o",
		APIKey:   "k",
		BaseURL:  "http://provider.example",
	}}
	if _, err := BuildRouter(models); err == nil || !strings.Contains(err.Error(), "https is required") {
		t.Errorf("err = %v, want remote HTTP TLS error", err)
	}
}

func TestBuildRouter_BaseURLCredentialsRejected(t *testing.T) {
	models := config.Models{Primary: config.ModelSpec{
		Provider: "openai",
		Model:    "gpt-4o",
		APIKey:   "k",
		BaseURL:  "https://user:pass@provider.example",
	}}
	if _, err := BuildRouter(models); err == nil || !strings.Contains(err.Error(), "user info is not allowed") {
		t.Errorf("err = %v, want URL credentials error", err)
	}
}

func TestBuildRouter_PassesStreamingIdleTimeout(t *testing.T) {
	models := config.Models{
		StreamingIdleTimeout: config.Duration(17 * time.Second),
		Primary: config.ModelSpec{
			Provider: "openai",
			Model:    "gpt-4o",
			APIKey:   "k",
			BaseURL:  "https://api.example",
		},
	}
	r, err := BuildRouter(models)
	if err != nil {
		t.Fatalf("BuildRouter: %v", err)
	}
	primary, ok := r.For("turn").(*OpenAIAdapter)
	if !ok {
		t.Fatalf("primary = %T, want *OpenAIAdapter", r.For("turn"))
	}
	if primary.streamingIdleTimeout != 17*time.Second {
		t.Errorf("idle timeout = %v, want 17s", primary.streamingIdleTimeout)
	}
}

func TestAdapterConstructorsRejectRemoteHTTP(t *testing.T) {
	if _, err := NewOpenAIAdapter("gpt-4o", "http://provider.example", "k", 0); err == nil {
		t.Fatal("NewOpenAIAdapter accepted remote HTTP endpoint")
	}
	if _, err := NewAnthropicAdapter("claude", "http://provider.example", "k", 0); err == nil {
		t.Fatal("NewAnthropicAdapter accepted remote HTTP endpoint")
	}
}
