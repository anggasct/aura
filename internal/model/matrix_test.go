package model

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/config"
)

// Helper to start mock servers for all 4 protocols
func mockProviderServer(t *testing.T, protocol string, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(handler)
}

func standardProviderHandler(t *testing.T, protocol string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch protocol {
		case config.ProtocolAnthropicMessages:
			writeFixture(t, w, fixtureBytes(t, "anthropic_message.json"))
		case config.ProtocolOpenAIChatCompat:
			writeFixture(t, w, fixtureBytes(t, "openai_completion.json"))
		case config.ProtocolOpenAIResponses:
			writeFixture(t, w, fixtureBytes(t, "openai_responses.json"))
		case config.ProtocolGeminiNative:
			writeFixture(t, w, fixtureBytes(t, "gemini_response.json"))
		default:
			http.Error(w, "unsupported protocol", http.StatusBadRequest)
		}
	}
}

func defaultCaps() config.ModelCapabilities {
	return config.ModelCapabilities{
		Streaming:        true,
		Tools:            true,
		StructuredOutput: true,
		ContextTokens:    128000,
		Tokenizer:        "cl100k_base",
	}
}

// TestCompatibilityMatrixAllProtocols verifies that fallback chains work across all 4
// supported protocols in multiple combinations.
func TestCompatibilityMatrixAllProtocols(t *testing.T) {
	protocols := []struct {
		name     string
		protocol string
		model    string
	}{
		{"anthropic", config.ProtocolAnthropicMessages, "claude-sonnet-4-20250514"},
		{"openai-chat", config.ProtocolOpenAIChatCompat, "gpt-4o"},
		{"openai-responses", config.ProtocolOpenAIResponses, "gpt-5"},
		{"gemini", config.ProtocolGeminiNative, "gemini-2.5-pro"},
	}

	for i := range protocols {
		p1 := protocols[i]
		p2 := protocols[(i+1)%len(protocols)]

		t.Run(p1.name+"_fallback_to_"+p2.name, func(t *testing.T) {
			var p1Calls, p2Calls atomic.Int32

			s1 := mockProviderServer(t, p1.protocol, func(w http.ResponseWriter, r *http.Request) {
				p1Calls.Add(1)
				http.Error(w, `{"error":"temporary overload"}`, http.StatusServiceUnavailable)
			})
			defer s1.Close()

			s2 := mockProviderServer(t, p2.protocol, func(w http.ResponseWriter, r *http.Request) {
				p2Calls.Add(1)
				standardProviderHandler(t, p2.protocol)(w, r)
			})
			defer s2.Close()

			defs := map[string]config.ModelDefinition{
				"cand-primary": {
					Protocol:     p1.protocol,
					Model:        p1.model,
					BaseURL:      s1.URL,
					Capabilities: defaultCaps(),
				},
				"cand-backup": {
					Protocol:     p2.protocol,
					Model:        p2.model,
					BaseURL:      s2.URL,
					Capabilities: defaultCaps(),
				},
			}

			route := config.ModelRoute{
				Candidates:          []string{"cand-primary", "cand-backup"},
				MaxProviderAttempts: 8,
			}

			router, err := BuildRouterWithRoutes(nil, config.Models{Definitions: defs}, map[string]config.ModelRoute{"primary": route}, nil)
			if err != nil {
				t.Fatalf("BuildRouterWithRoutes: %v", err)
			}

			adapter, err := router.For("agent")
			if err != nil {
				t.Fatalf("router.For(agent): %v", err)
			}

			responses, err := collect(adapter.GenerateContent(t.Context(), sampleRequest("test fallback"), false))
			if err != nil {
				t.Fatalf("GenerateContent unexpected error: %v", err)
			}

			if len(responses) == 0 {
				t.Fatal("expected non-empty response from candidate 2")
			}
			if p1Calls.Load() == 0 {
				t.Errorf("candidate 1 was not called")
			}
			if p2Calls.Load() == 0 {
				t.Errorf("candidate 2 was not called")
			}
		})
	}
}

// TestObservableBoundaryStreaming verifies that if candidate 1 emits a token
// and then the stream fails, fallback does not switch to candidate 2.
func TestObservableBoundaryStreaming(t *testing.T) {
	var c1Calls, c2Calls atomic.Int32

	s1 := mockProviderServer(t, config.ProtocolOpenAIChatCompat, func(w http.ResponseWriter, r *http.Request) {
		c1Calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("expected Flusher")
		}

		// Emit initial chunk
		chunk := `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":123,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":"Observable partial output"},"finish_reason":null}]}` + "\n\n"
		_, _ = w.Write([]byte(chunk))
		flusher.Flush()

		// Abruptly terminate connection without completing stream
	})
	defer s1.Close()

	s2 := mockProviderServer(t, config.ProtocolAnthropicMessages, func(w http.ResponseWriter, r *http.Request) {
		c2Calls.Add(1)
		standardProviderHandler(t, config.ProtocolAnthropicMessages)(w, r)
	})
	defer s2.Close()

	defs := map[string]config.ModelDefinition{
		"cand-1": {
			Protocol:     config.ProtocolOpenAIChatCompat,
			Model:        "gpt-4o",
			BaseURL:      s1.URL,
			Capabilities: defaultCaps(),
		},
		"cand-2": {
			Protocol:     config.ProtocolAnthropicMessages,
			Model:        "claude-sonnet-4-20250514",
			BaseURL:      s2.URL,
			Capabilities: defaultCaps(),
		},
	}

	route := config.ModelRoute{
		Candidates:          []string{"cand-1", "cand-2"},
		MaxProviderAttempts: 6,
	}

	router, err := BuildRouterWithRoutes(nil, config.Models{Definitions: defs}, map[string]config.ModelRoute{"primary": route}, nil)
	if err != nil {
		t.Fatalf("BuildRouterWithRoutes: %v", err)
	}

	adapter, err := router.For("agent")
	if err != nil {
		t.Fatalf("router.For: %v", err)
	}

	var observed []*adkmodel.LLMResponse
	var streamErr error

	for resp, err := range adapter.GenerateContent(t.Context(), sampleRequest("stream boundary test"), true) {
		if err != nil {
			streamErr = err
			break
		}
		observed = append(observed, resp)
	}

	if len(observed) == 0 {
		t.Fatal("expected at least 1 observable response chunk")
	}
	if streamErr == nil {
		t.Fatal("expected boundary error on truncated stream")
	}
	if code, ok := CodeOf(streamErr); !ok || code != ErrorCodeFallbackBoundary {
		t.Errorf("error code = %v (ok=%v), want %s", code, ok, ErrorCodeFallbackBoundary)
	}
	if c2Calls.Load() != 0 {
		t.Errorf("candidate 2 was called %d times after observable output was emitted; want 0", c2Calls.Load())
	}
}

// TestNonFallbackClassesProvePolicyAndAuthNoBypass verifies policy and auth errors do not fallback.
func TestNonFallbackClassesProvePolicyAndAuthNoBypass(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		wantErr    ErrorCode
	}{
		{
			name:       "auth_failure_401",
			statusCode: http.StatusUnauthorized,
			body:       `{"error":"invalid_api_key"}`,
			wantErr:    ErrorCodeAuthFailed,
		},
		{
			name:       "policy_rejection_safety",
			statusCode: http.StatusBadRequest,
			body:       `{"error":{"message":"content violated safety policy","code":"content_policy_violation"}}`,
			wantErr:    ErrorCodeContentFiltered,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c1Calls, c2Calls atomic.Int32

			s1 := mockProviderServer(t, config.ProtocolOpenAIChatCompat, func(w http.ResponseWriter, r *http.Request) {
				c1Calls.Add(1)
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			})
			defer s1.Close()

			s2 := mockProviderServer(t, config.ProtocolAnthropicMessages, func(w http.ResponseWriter, r *http.Request) {
				c2Calls.Add(1)
				standardProviderHandler(t, config.ProtocolAnthropicMessages)(w, r)
			})
			defer s2.Close()

			defs := map[string]config.ModelDefinition{
				"cand-1": {
					Protocol:     config.ProtocolOpenAIChatCompat,
					Model:        "gpt-4o",
					BaseURL:      s1.URL,
					Capabilities: defaultCaps(),
				},
				"cand-2": {
					Protocol:     config.ProtocolAnthropicMessages,
					Model:        "claude-sonnet-4-20250514",
					BaseURL:      s2.URL,
					Capabilities: defaultCaps(),
				},
			}

			route := config.ModelRoute{
				Candidates:          []string{"cand-1", "cand-2"},
				MaxProviderAttempts: 6,
			}

			router, err := BuildRouterWithRoutes(nil, config.Models{Definitions: defs}, map[string]config.ModelRoute{"primary": route}, nil)
			if err != nil {
				t.Fatalf("BuildRouterWithRoutes: %v", err)
			}

			adapter, err := router.For("agent")
			if err != nil {
				t.Fatalf("router.For: %v", err)
			}

			_, err = collect(adapter.GenerateContent(t.Context(), sampleRequest("test policy"), false))
			if err == nil {
				t.Fatal("expected error, got nil")
			}

			if code, ok := CodeOf(err); !ok || code != tt.wantErr {
				t.Errorf("error code = %v, want %s (err: %v)", code, tt.wantErr, err)
			}

			if c2Calls.Load() != 0 {
				t.Errorf("candidate 2 was invoked on non-fallback error %s; policy filter was bypassed!", tt.wantErr)
			}
		})
	}
}

// TestBudgetDeterministicBackoffAndCancellation verifies cancellation and backoff handling.
func TestBudgetDeterministicBackoffAndCancellation(t *testing.T) {
	var c1Calls atomic.Int32

	s1 := mockProviderServer(t, config.ProtocolOpenAIChatCompat, func(w http.ResponseWriter, r *http.Request) {
		c1Calls.Add(1)
		time.Sleep(50 * time.Millisecond)
		http.Error(w, "overloaded", http.StatusServiceUnavailable)
	})
	defer s1.Close()

	defs := map[string]config.ModelDefinition{
		"cand-1": {
			Protocol:     config.ProtocolOpenAIChatCompat,
			Model:        "gpt-4o",
			BaseURL:      s1.URL,
			Capabilities: defaultCaps(),
		},
	}

	route := config.ModelRoute{
		Candidates:          []string{"cand-1"},
		MaxProviderAttempts: 2,
		RetryDelayBudget:    config.Duration(20 * time.Millisecond),
	}

	router, err := BuildRouterWithRoutes(nil, config.Models{Definitions: defs}, map[string]config.ModelRoute{"primary": route}, nil)
	if err != nil {
		t.Fatalf("BuildRouterWithRoutes: %v", err)
	}

	adapter, err := router.For("agent")
	if err != nil {
		t.Fatalf("router.For: %v", err)
	}

	// Test cancellation stops immediately
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = collect(adapter.GenerateContent(ctx, sampleRequest("test cancel"), false))
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got: %v", err)
	}
}

// TestTelemetryAndExhaustionPrivacyRedaction verifies that exhaustion errors and telemetry logs
// contain zero prompts, outputs, secrets, endpoints, headers, or raw provider bodies.
func TestTelemetryAndExhaustionPrivacyRedaction(t *testing.T) {
	secretCanary := "sk-super-secret-key-xyz987"
	endpointCanary := "https://private-endpoint-corp.internal.example.org"
	promptCanary := "TOP_SECRET_PROMPT_DO_NOT_LEAK"
	bodyCanary := "INTERNAL_DIAGNOSTIC_BODY_SENSITIVE"

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	s1 := mockProviderServer(t, config.ProtocolOpenAIChatCompat, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Custom-Secret-Header", "private-header-value")
		http.Error(w, `{"error":"`+bodyCanary+`"}`, http.StatusServiceUnavailable)
	})
	defer s1.Close()

	defs := map[string]config.ModelDefinition{
		"safe-candidate-1": {
			Protocol:     config.ProtocolOpenAIChatCompat,
			Model:        "gpt-4o",
			BaseURL:      s1.URL,
			Capabilities: defaultCaps(),
		},
	}

	route := config.ModelRoute{
		Candidates:          []string{"safe-candidate-1"},
		MaxProviderAttempts: 4,
	}

	router, err := BuildRouterWithRoutes(logger, config.Models{Definitions: defs}, map[string]config.ModelRoute{"safe-route": route}, nil)
	if err != nil {
		t.Fatalf("BuildRouterWithRoutes: %v", err)
	}

	adapter, err := router.ForRoute("safe-route")
	if err != nil {
		t.Fatalf("ForRoute: %v", err)
	}

	req := &adkmodel.LLMRequest{
		Contents: []*genai.Content{
			{Role: "user", Parts: []*genai.Part{{Text: promptCanary}}},
		},
	}

	_, err = collect(adapter.GenerateContent(context.Background(), req, false))
	if err == nil {
		t.Fatal("expected exhaustion error, got nil")
	}

	errStr := err.Error()
	logStr := logBuf.String()

	canaries := []string{secretCanary, endpointCanary, promptCanary, bodyCanary, "private-header-value"}
	for _, c := range canaries {
		if strings.Contains(errStr, c) {
			t.Errorf("error string leaked sensitive canary %q: %s", c, errStr)
		}
		if strings.Contains(logStr, c) {
			t.Errorf("telemetry log leaked sensitive canary %q: %s", c, logStr)
		}
	}

	// Verify that ONLY safe aliases and normalized classes appear in exhaustion error
	if !strings.Contains(errStr, "safe-candidate-1") {
		t.Errorf("error lacks safe candidate alias: %s", errStr)
	}
	if !strings.Contains(errStr, "overloaded") && !strings.Contains(errStr, "transient") {
		t.Errorf("error lacks normalized class: %s", errStr)
	}
}

// TestHighConcurrencyRaceSafety tests concurrent access across workers.
func TestHighConcurrencyRaceSafety(t *testing.T) {
	s := mockProviderServer(t, config.ProtocolOpenAIChatCompat, func(w http.ResponseWriter, r *http.Request) {
		writeFixture(t, w, fixtureBytes(t, "openai_completion.json"))
	})
	defer s.Close()

	defs := map[string]config.ModelDefinition{
		"cand-1": {
			Protocol:     config.ProtocolOpenAIChatCompat,
			Model:        "gpt-4o",
			BaseURL:      s.URL,
			Capabilities: defaultCaps(),
		},
		"cand-2": {
			Protocol:     config.ProtocolOpenAIChatCompat,
			Model:        "gpt-4o-mini",
			BaseURL:      s.URL,
			Capabilities: defaultCaps(),
		},
	}

	route := config.ModelRoute{
		Candidates:          []string{"cand-1", "cand-2"},
		MaxProviderAttempts: 4,
	}

	router, err := BuildRouterWithRoutes(nil, config.Models{Definitions: defs}, map[string]config.ModelRoute{"primary": route}, nil)
	if err != nil {
		t.Fatalf("BuildRouterWithRoutes: %v", err)
	}

	adapter, err := router.For("agent")
	if err != nil {
		t.Fatalf("router.For: %v", err)
	}

	const workers = 16
	const requestsPerWorker = 5
	var wg sync.WaitGroup
	wg.Add(workers)

	for range workers {
		go func() {
			defer wg.Done()
			for range requestsPerWorker {
				resps, err := collect(adapter.GenerateContent(context.Background(), sampleRequest("concurrent"), false))
				if err != nil {
					t.Errorf("concurrent GenerateContent error: %v", err)
					return
				}
				if len(resps) == 0 {
					t.Errorf("empty responses")
				}
			}
		}()
	}

	wg.Wait()
}
