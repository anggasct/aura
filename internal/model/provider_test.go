package model

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/anggasct/aura/internal/config"
)

type capturedRequest struct {
	session   string
	userAgent string
	auth      string
	tenant    string
}

func providerCaptureServer(t *testing.T, mu *sync.Mutex, captured *[]capturedRequest, fixture string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*captured = append(*captured, capturedRequest{
			session:   r.Header.Get(config.SessionHeaderName),
			userAgent: r.Header.Get("User-Agent"),
			auth:      r.Header.Get("Authorization"),
			tenant:    r.Header.Get("X-Tenant"),
		})
		mu.Unlock()
		writeFixture(t, w, fixtureBytes(t, fixture))
	}))
}

func mustProviderAdapter(t *testing.T, name string, spec *config.ModelDefinition, logger *slog.Logger, providers map[string]config.ProviderPreset) adkmodel.LLM {
	t.Helper()
	t.Setenv("AURA_PROVIDER_TEST_KEY", "test-key")
	spec.APIKeyEnv = "AURA_PROVIDER_TEST_KEY"
	if logger == nil {
		logger = slog.Default()
	}
	adapter, configured, err := newAdapter(logger, name, spec, providers, 0, 0)
	if err != nil {
		t.Fatalf("newAdapter: %v", err)
	}
	if !configured {
		t.Fatalf("newAdapter(%s): not configured", name)
	}
	return adapter
}

func lastCaptured(mu *sync.Mutex, captured *[]capturedRequest) capturedRequest {
	mu.Lock()
	defer mu.Unlock()
	if len(*captured) == 0 {
		return capturedRequest{}
	}
	return (*captured)[len(*captured)-1]
}

func TestProviderSessionHeader_Stability(t *testing.T) {
	var mu sync.Mutex
	var captured []capturedRequest
	srv := providerCaptureServer(t, &mu, &captured, "openai_responses.json")
	defer srv.Close()

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	adapter := mustProviderAdapter(t, "primary", &config.ModelDefinition{
		Provider: "opencode-go",
		Model:    "muse-spark-1.3-contributor",
		BaseURL:  srv.URL,
	}, logger, nil)

	for _, sessionID := range []string{"sess-A", "sess-A", "sess-B"} {
		ctx := WithSessionID(t.Context(), sessionID)
		if _, err := collect(adapter.GenerateContent(ctx, sampleRequest("hi"), false)); err != nil {
			t.Fatalf("GenerateContent(session %s): %v", sessionID, err)
		}
		if got := lastCaptured(&mu, &captured).session; got != sessionID {
			t.Fatalf("session header = %q, want %q", got, sessionID)
		}
	}

	mu.Lock()
	got := make([]capturedRequest, len(captured))
	copy(got, captured)
	mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("captured %d requests, want 3", len(got))
	}
	if got[0].session == "" || got[0].session != got[1].session {
		t.Errorf("session not stable across turns: %q vs %q", got[0].session, got[1].session)
	}
	if got[0].session == got[2].session {
		t.Errorf("session not distinct across conversations: %q", got[2].session)
	}
	for i, c := range got {
		if !strings.HasPrefix(c.userAgent, "aura") {
			t.Errorf("request %d User-Agent = %q, want aura-identifying", i, c.userAgent)
		}
		if c.userAgent == "Go-http-client/1.1" {
			t.Errorf("request %d carries the generic HTTP default User-Agent", i)
		}
		if c.auth != "Bearer test-key" {
			t.Errorf("request %d Authorization = %q", i, c.auth)
		}
	}
	if !strings.Contains(logs.String(), `"provider":"opencode-go"`) {
		t.Errorf("telemetry lacks provider name:\n%s", logs.String())
	}
}

func TestProviderSessionHeader_MissingFails(t *testing.T) {
	var mu sync.Mutex
	var captured []capturedRequest
	srv := providerCaptureServer(t, &mu, &captured, "openai_responses.json")
	defer srv.Close()

	adapter := mustProviderAdapter(t, "primary", &config.ModelDefinition{
		Provider: "opencode-go",
		Model:    "muse-spark-1.3-contributor",
		BaseURL:  srv.URL,
	}, nil, nil)

	_, err := collect(adapter.GenerateContent(t.Context(), sampleRequest("hi"), false))
	if err == nil {
		t.Fatalf("GenerateContent without session: got nil error, want failure")
	}
	if code, ok := CodeOf(err); !ok || code != ErrorCodeProtocolInvalid {
		t.Fatalf("code = %q, %v; want %q", code, ok, ErrorCodeProtocolInvalid)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 0 {
		t.Fatalf("request reached the provider without a session identity")
	}
}

func TestProviderCustomHeaders_Sent(t *testing.T) {
	var mu sync.Mutex
	var captured []capturedRequest
	srv := providerCaptureServer(t, &mu, &captured, "openai_completion.json")
	defer srv.Close()

	custom := config.ProviderPreset{
		Protocol:        config.ProtocolOpenAIChatCompat,
		BaseURL:         srv.URL,
		AuthScheme:      config.ProviderAuthBearer,
		Headers:         map[string]string{"X-Tenant": "tenant-42"},
		SessionBehavior: config.ProviderSessionNone,
	}
	adapter := mustProviderAdapter(t, "primary", &config.ModelDefinition{
		Model:  "custom-model",
		Custom: &custom,
	}, nil, nil)

	if _, err := collect(adapter.GenerateContent(t.Context(), sampleRequest("hi"), false)); err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	got := lastCaptured(&mu, &captured)
	if got.tenant != "tenant-42" {
		t.Errorf("X-Tenant = %q, want tenant-42", got.tenant)
	}
	if got.session != "" {
		t.Errorf("session header = %q, want absent", got.session)
	}
	if strings.HasPrefix(got.userAgent, "aura") {
		t.Errorf("User-Agent = %q, want unchanged default for custom endpoints", got.userAgent)
	}
	if got.auth != "Bearer test-key" {
		t.Errorf("Authorization = %q", got.auth)
	}
}

func TestProviderLegacyHeaders_Unchanged(t *testing.T) {
	var mu sync.Mutex
	var captured []capturedRequest
	srv := providerCaptureServer(t, &mu, &captured, "openai_completion.json")
	defer srv.Close()

	adapter := mustProviderAdapter(t, "primary", &config.ModelDefinition{
		Protocol: config.ProtocolOpenAIChatCompat,
		Model:    "gpt-4o",
		BaseURL:  srv.URL,
	}, nil, nil)

	if _, err := collect(adapter.GenerateContent(WithSessionID(t.Context(), "sess-A"), sampleRequest("hi"), false)); err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	got := lastCaptured(&mu, &captured)
	if got.session != "" {
		t.Errorf("session header = %q, want absent on the legacy path", got.session)
	}
	if strings.HasPrefix(got.userAgent, "aura") {
		t.Errorf("User-Agent = %q, want unchanged default on the legacy path", got.userAgent)
	}
}

func TestOpencodeGoPreset_EndpointSingleV1(t *testing.T) {
	resolved, err := config.Models{}.ResolveDefinition(&config.ModelDefinition{
		Provider: "opencode-go",
		Model:    "muse-spark-1.3-contributor",
	})
	if err != nil {
		t.Fatalf("ResolveDefinition: %v", err)
	}
	if resolved.BaseURL != "https://opencode.ai/zen/go" {
		t.Fatalf("BaseURL = %q, want %q", resolved.BaseURL, "https://opencode.ai/zen/go")
	}
	endpoint := openAIResponsesCodec{}.endpoint(resolved.BaseURL, sampleRequest("hi"), false)
	if endpoint != "https://opencode.ai/zen/go/v1/responses" {
		t.Fatalf("endpoint = %q, want %q", endpoint, "https://opencode.ai/zen/go/v1/responses")
	}
	if strings.Contains(endpoint, "/v1/v1/") {
		t.Fatalf("endpoint %q contains a doubled /v1 path segment", endpoint)
	}

	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		writeFixture(t, w, fixtureBytes(t, "openai_responses.json"))
	}))
	defer srv.Close()

	providers := map[string]config.ProviderPreset{
		"opencode-go": {
			Protocol:        config.ProtocolOpenAIResponses,
			BaseURL:         srv.URL,
			AuthScheme:      config.ProviderAuthBearer,
			SessionBehavior: config.ProviderSessionOpencode,
		},
	}
	adapter := mustProviderAdapter(t, "primary", &config.ModelDefinition{
		Provider: "opencode-go",
		Model:    "muse-spark-1.3-contributor",
	}, nil, providers)
	if _, err := collect(adapter.GenerateContent(WithSessionID(t.Context(), "sess-regression"), sampleRequest("hi"), false)); err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 1 {
		t.Fatalf("captured %d requests, want 1", len(paths))
	}
	if paths[0] != "/v1/responses" {
		t.Errorf("path = %q, want %q (no doubled /v1 segment)", paths[0], "/v1/responses")
	}
}

func TestOpencodeGoPreset_BaseURLOverrideStillWins(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		writeFixture(t, w, fixtureBytes(t, "openai_responses.json"))
	}))
	defer srv.Close()

	adapter := mustProviderAdapter(t, "primary", &config.ModelDefinition{
		Provider: "opencode-go",
		Model:    "muse-spark-1.3-contributor",
		BaseURL:  srv.URL,
	}, nil, nil)
	if _, err := collect(adapter.GenerateContent(WithSessionID(t.Context(), "sess-override"), sampleRequest("hi"), false)); err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 1 {
		t.Fatalf("captured %d requests, want 1", len(paths))
	}
	if paths[0] != "/v1/responses" {
		t.Errorf("path = %q, want %q", paths[0], "/v1/responses")
	}
}
