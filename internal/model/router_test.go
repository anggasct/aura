package model

import (
	"context"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/anggasct/aura/internal/config"
)

type stubLLM struct {
	name string
}

func configuredDefinition(protocol, name, baseURL string) config.ModelDefinition {
	return config.ModelDefinition{
		Protocol:  protocol,
		Model:     name,
		BaseURL:   baseURL,
		APIKeyEnv: "TEST_MODEL_API_KEY",
	}
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
	if got, err := r.For("turn"); err != nil || got != primary {
		t.Errorf("For(turn) = %v, %v; want primary", got, err)
	}
	if got, err := r.For(""); err != nil || got != primary {
		t.Errorf("For(empty) = %v, %v; want primary", got, err)
	}
	for _, task := range []string{"summarize", "title", "curation", "compression", "profiling"} {
		if got, err := r.For(task); err != nil || got != auxiliary {
			t.Errorf("For(%s) = %v, %v; want auxiliary", task, got, err)
		}
	}
	if got, err := r.For("agent"); err != nil || got != primary {
		t.Errorf("For(agent) = %v, %v; want primary", got, err)
	}
	if _, err := r.For("unknown_task"); err == nil {
		t.Error("For(unknown_task) accepted an unknown task")
	} else {
		wantCode(t, err, ErrorCodeNotFound)
	}
}

func TestRouter_RoutingOverridesMergeWithDefaults(t *testing.T) {
	primary := &stubLLM{name: "primary"}
	auxiliary := &stubLLM{name: "auxiliary"}
	r, err := NewRouter(primary, auxiliary, map[string]string{"summarize": "primary"})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	if got, err := r.For("summarize"); err != nil || got != primary {
		t.Errorf("For(summarize) = %v, %v; want primary (override)", got, err)
	}
	if got, err := r.For("title"); err != nil || got != auxiliary {
		t.Errorf("For(title) = %v, %v; want auxiliary (default preserved)", got, err)
	}
	if got, err := r.For("compression"); err != nil || got != auxiliary {
		t.Errorf("For(compression) = %v, %v; want auxiliary (default preserved)", got, err)
	}
}

func TestBuildRouter_OpenRouterAndOllamaEndpoints(t *testing.T) {
	t.Setenv("TEST_MODEL_API_KEY", "k")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeFixture(t, w, fixtureBytes(t, "openai_completion.json"))
	}))
	defer srv.Close()

	models := config.Models{
		Definitions: map[string]config.ModelDefinition{
			"primary":   configuredDefinition(config.ProtocolOpenAIChatCompat, "deepseek-r1", srv.URL),
			"auxiliary": configuredDefinition(config.ProtocolOpenAIChatCompat, "llama3", "http://localhost:11434"),
		},
	}
	r, err := BuildRouter(nil, models)
	if err != nil {
		t.Fatalf("BuildRouter: %v", err)
	}
	primary, err := r.For("turn")
	if err != nil {
		t.Fatalf("For(turn): %v", err)
	}
	resps, err := collect(primary.GenerateContent(context.Background(), sampleRequest("hi"), false))
	if err != nil {
		t.Fatalf("openrouter request: %v", err)
	}
	if len(resps) != 1 || resps[0].Content.Parts[0].Text != "Hello from the model." {
		t.Errorf("unexpected response: %+v", resps)
	}
	auxiliary, err := r.For("summarize")
	if err != nil {
		t.Fatalf("For(summarize): %v", err)
	}
	if _, ok := auxiliary.(*OpenAIAdapter); !ok {
		t.Errorf("ollama auxiliary = %T, want *OpenAIAdapter", auxiliary)
	}
}

func wantCode(t *testing.T, err error, want ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with code %s, got nil", want)
	}
	got, ok := CodeOf(err)
	if !ok {
		t.Fatalf("error %v carries no model code, want %s", err, want)
	}
	if got != want {
		t.Errorf("error code = %s, want %s", got, want)
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
	t.Setenv("TEST_MODEL_API_KEY", "k")
	models := config.Models{
		Definitions: map[string]config.ModelDefinition{
			"primary":   configuredDefinition(config.ProtocolAnthropicMessages, "claude", "https://api.anthropic.com"),
			"auxiliary": configuredDefinition(config.ProtocolOpenAIChatCompat, "llama", "https://openrouter.ai/api"),
		},
	}
	r, err := BuildRouter(nil, models)
	if err != nil {
		t.Fatalf("BuildRouter: %v", err)
	}
	primary, err := r.For("turn")
	if err != nil {
		t.Fatalf("For(turn): %v", err)
	}
	if _, ok := primary.(*AnthropicAdapter); !ok {
		t.Errorf("primary = %T, want *AnthropicAdapter", primary)
	}
	auxiliary, err := r.For("summarize")
	if err != nil {
		t.Fatalf("For(summarize): %v", err)
	}
	if _, ok := auxiliary.(*OpenAIAdapter); !ok {
		t.Errorf("auxiliary = %T, want *OpenAIAdapter", auxiliary)
	}
}

func TestBuildRouter_UnconfiguredProviderFailsClosed(t *testing.T) {
	r, err := BuildRouter(nil, config.Models{})
	if err != nil {
		t.Fatalf("BuildRouter: %v", err)
	}
	if m, err := r.For("turn"); err == nil || m != nil {
		t.Errorf("For(turn) = %v, %v; want typed not_found error", m, err)
	}
	if m, err := r.For("summarize"); err == nil || m != nil {
		t.Errorf("For(summarize) = %v, %v; want typed not_found error", m, err)
	}
}

func TestBuildRouter_UnsupportedProviderFails(t *testing.T) {
	t.Setenv("TEST_MODEL_API_KEY", "k")
	models := config.Models{Definitions: map[string]config.ModelDefinition{
		"primary": configuredDefinition("bogus", "x", "https://provider.example"),
	}}
	if _, err := BuildRouter(nil, models); err == nil || !strings.Contains(err.Error(), "unsupported protocol") {
		t.Errorf("err = %v, want unsupported protocol error", err)
	}
}

func TestBuildRouter_MissingKeyValidation(t *testing.T) {
	models := config.Models{Definitions: map[string]config.ModelDefinition{
		"primary": {Protocol: config.ProtocolAnthropicMessages, Model: "claude", BaseURL: "https://api.anthropic.com"},
	}}
	if _, err := BuildRouter(nil, models); err == nil || !strings.Contains(err.Error(), "api_key_env or api_key_file") {
		t.Errorf("err = %v, want secret reference error", err)
	}

	localhost := config.Models{Definitions: map[string]config.ModelDefinition{
		"primary": {Protocol: config.ProtocolOpenAIChatCompat, Model: "llama3", BaseURL: "http://localhost:11434"},
	}}
	if _, err := BuildRouter(nil, localhost); err != nil {
		t.Errorf("localhost without key should be allowed: %v", err)
	}
}

func TestBuildRouter_InvalidBaseURLFails(t *testing.T) {
	models := config.Models{Definitions: map[string]config.ModelDefinition{
		"primary": {Protocol: config.ProtocolOpenAIChatCompat, Model: "gpt-4o", APIKeyEnv: "TEST_MODEL_API_KEY", BaseURL: "://bad"},
	}}
	if _, err := BuildRouter(nil, models); err == nil || !strings.Contains(err.Error(), "invalid base_url") {
		t.Errorf("err = %v, want invalid base_url error", err)
	}
}

func TestBuildRouter_RemoteHTTPRequiresTLS(t *testing.T) {
	models := config.Models{Definitions: map[string]config.ModelDefinition{
		"primary": {Protocol: config.ProtocolOpenAIChatCompat, Model: "gpt-4o", APIKeyEnv: "TEST_MODEL_API_KEY", BaseURL: "http://provider.example"},
	}}
	if _, err := BuildRouter(nil, models); err == nil || !strings.Contains(err.Error(), "https is required") {
		t.Errorf("err = %v, want remote HTTP TLS error", err)
	}
}

func TestBuildRouter_BaseURLCredentialsRejected(t *testing.T) {
	models := config.Models{Definitions: map[string]config.ModelDefinition{
		"primary": {Protocol: config.ProtocolOpenAIChatCompat, Model: "gpt-4o", APIKeyEnv: "TEST_MODEL_API_KEY", BaseURL: "https://user:pass@provider.example"},
	}}
	if _, err := BuildRouter(nil, models); err == nil || !strings.Contains(err.Error(), "user info is not allowed") {
		t.Errorf("err = %v, want URL credentials error", err)
	}
}

func TestBuildRouter_PassesStreamingIdleTimeout(t *testing.T) {
	t.Setenv("TEST_MODEL_API_KEY", "k")
	models := config.Models{
		StreamingIdleTimeout: config.Duration(17 * time.Second),
		Definitions: map[string]config.ModelDefinition{
			"primary": configuredDefinition(config.ProtocolOpenAIChatCompat, "gpt-4o", "https://api.example"),
		},
	}
	r, err := BuildRouter(nil, models)
	if err != nil {
		t.Fatalf("BuildRouter: %v", err)
	}
	primary, err := r.For("turn")
	if err != nil {
		t.Fatalf("For(turn): %v", err)
	}
	openaiPrimary, ok := primary.(*OpenAIAdapter)
	if !ok {
		t.Fatalf("primary = %T, want *OpenAIAdapter", primary)
	}
	if openaiPrimary.core.idleTimeout != 17*time.Second {
		t.Errorf("idle timeout = %v, want 17s", openaiPrimary.core.idleTimeout)
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

func TestResolveSecretFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api-key")
	if err := os.WriteFile(path, []byte(" file-secret\n"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	got, err := resolveSecret("primary", &config.ModelDefinition{APIKeyFile: path})
	if err != nil {
		t.Fatalf("resolveSecret: %v", err)
	}
	if got != "file-secret" {
		t.Errorf("secret = %q, want file-secret", got)
	}
}

func TestRejectCrossOriginRedirect(t *testing.T) {
	previous, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://provider.example/v1", http.NoBody)
	if err != nil {
		t.Fatalf("previous request: %v", err)
	}
	sameOrigin, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://provider.example/other", http.NoBody)
	if err != nil {
		t.Fatalf("same-origin request: %v", err)
	}
	if err := rejectCrossOriginRedirect(sameOrigin, []*http.Request{previous}); err != nil {
		t.Fatalf("same-origin redirect rejected: %v", err)
	}
	crossOrigin, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://attacker.example/collect", http.NoBody)
	if err != nil {
		t.Fatalf("cross-origin request: %v", err)
	}
	if err := rejectCrossOriginRedirect(crossOrigin, []*http.Request{previous}); err == nil {
		t.Fatal("cross-origin redirect was accepted")
	}
}
