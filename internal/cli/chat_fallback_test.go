package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/model"
)

func chatTestConfig(t *testing.T, suffix, failingURL, backupURL string) (cfg *config.Config, routeName string) {
	t.Helper()
	routeName = "primary-" + suffix
	return &config.Config{
		Models: config.Models{
			Definitions: map[string]config.ModelDefinition{
				"failing": {
					Protocol:     config.ProtocolOpenAIChatCompat,
					Model:        "failing-model-" + suffix,
					BaseURL:      failingURL,
					Capabilities: config.ModelCapabilities{ContextTokens: 128000, Tokenizer: "cl100k_base", Streaming: true, Tools: true},
				},
				"backup": {
					Protocol:     config.ProtocolAnthropicMessages,
					Model:        "backup-model-" + suffix,
					BaseURL:      backupURL,
					Capabilities: config.ModelCapabilities{ContextTokens: 128000, Tokenizer: "cl100k_base", Streaming: true, Tools: true},
				},
			},
		},
		ModelRoutes: map[string]config.ModelRoute{
			routeName: {Candidates: []string{"failing", "backup"}},
		},
	}, routeName
}

func TestChatWireFailsOverThroughRoute(t *testing.T) {
	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)

	failingHits := 0
	failingSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failingHits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"overloaded","type":"server_error","code":"overloaded"}}`))
	}))
	defer failingSrv.Close()

	backupHits := 0
	backupSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"backup-model","content":[{"type":"text","text":"recovered via backup"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":4}}`))
	}))
	defer backupSrv.Close()

	cfg, routeName := chatTestConfig(t, suffix, failingSrv.URL, backupSrv.URL)
	route := cfg.ModelRoutes[routeName]
	route.MaxProviderAttempts = 8
	cfg.ModelRoutes[routeName] = route

	if err := model.RegisterAdaptersWithRoutes(context.Background(), nil, cfg.Models, cfg.ModelRoutes, nil, nil); err != nil {
		t.Fatalf("RegisterAdaptersWithRoutes: %v", err)
	}

	routeModel, err := model.RouteModelName(cfg, cfg.Models.Definitions["failing"].Model)
	if err != nil {
		t.Fatalf("RouteModelName: %v", err)
	}
	modelName, err := routeModel(routeName)
	if err != nil {
		t.Fatalf("resolve route: %v", err)
	}
	if modelName != routeName {
		t.Fatalf("route resolved to %q, want the fallback route name %q", modelName, routeName)
	}

	resolved, err := adkmodel.NewLLM(context.Background(), modelName)
	if err != nil {
		t.Fatalf("resolve %q through the model registry: %v", modelName, err)
	}

	req := &adkmodel.LLMRequest{
		Contents: []*genai.Content{{
			Role:  "user",
			Parts: []*genai.Part{{Text: "hello"}},
		}},
	}
	var gotText strings.Builder
	for resp, err := range resolved.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatalf("turn through route: %v", err)
		}
		if resp != nil && resp.Content != nil {
			for _, part := range resp.Content.Parts {
				if part != nil {
					gotText.WriteString(part.Text)
				}
			}
		}
	}
	if failingHits == 0 {
		t.Fatal("candidate 1 endpoint was never called; the route did not start at candidate 1")
	}
	if backupHits == 0 {
		t.Fatal("candidate 2 endpoint was never called; failover did not happen")
	}
	if gotText.String() != "recovered via backup" {
		t.Fatalf("turn completed with %q, want the candidate 2 answer", gotText.String())
	}
}

func TestChatWireUnknownRouteFailsClosed(t *testing.T) {
	cfg, _ := chatTestConfig(t, "failclosed", "http://127.0.0.1:1", "http://127.0.0.1:2")
	routeModel, err := model.RouteModelName(cfg, "fallback-model")
	if err != nil {
		t.Fatalf("RouteModelName: %v", err)
	}
	if _, err := routeModel("no-such-route"); err == nil {
		t.Fatal("unknown route resolved without error, want fail-closed")
	}
	if got, err := routeModel("failing"); err != nil || got != "failing-model-failclosed" {
		t.Errorf("definition route resolution = %q, %v; want failing-model-failclosed, nil", got, err)
	}
	if got, err := routeModel("backup"); err != nil || got != "backup-model-failclosed" {
		t.Errorf("definition route resolution = %q, %v; want backup-model-failclosed, nil", got, err)
	}
}

func TestChatWireCostBudget(t *testing.T) {
	cases := []struct {
		name          string
		costBudgetUSD float64
		inputTokens   int64
		outputTokens  int64
		wantErrCode   model.ErrorCode
	}{
		{
			name:          "exceeded",
			costBudgetUSD: 0.0001,
			inputTokens:   100,
			outputTokens:  200,
			wantErrCode:   model.ErrorCodeBudgetExceeded,
		},
		{
			name:          "under_budget",
			costBudgetUSD: 10.0,
			inputTokens:   100,
			outputTokens:  200,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
			serverHits := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				serverHits++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"priced-model","content":[{"type":"text","text":"budget verified"}],"stop_reason":"end_turn","usage":{"input_tokens":` + strconv.FormatInt(tc.inputTokens, 10) + `,"output_tokens":` + strconv.FormatInt(tc.outputTokens, 10) + `}}`))
			}))
			defer srv.Close()

			routeName := "priced-route-" + suffix
			cfg := &config.Config{
				Models: config.Models{
					Definitions: map[string]config.ModelDefinition{
						"priced": {
							Protocol: config.ProtocolAnthropicMessages,
							Model:    "priced-model-" + suffix,
							BaseURL:  srv.URL,
							Capabilities: config.ModelCapabilities{
								ContextTokens:        128000,
								Tokenizer:            "cl100k_base",
								Streaming:            true,
								Tools:                true,
								MicrosPerInputToken:  1000,
								MicrosPerOutputToken: 2000,
							},
						},
					},
				},
				ModelRoutes: map[string]config.ModelRoute{
					routeName: {
						Candidates:    []string{"priced"},
						CostBudgetUSD: tc.costBudgetUSD,
					},
				},
			}

			prices, err := openPriceRegistry(t.Context(), nil, cfg, "", "")
			if err != nil {
				t.Fatalf("openPriceRegistry: %v", err)
			}

			if err := model.RegisterAdaptersWithRoutes(t.Context(), nil, cfg.Models, cfg.ModelRoutes, nil, prices); err != nil {
				t.Fatalf("RegisterAdaptersWithRoutes: %v", err)
			}

			routeModel, err := model.RouteModelName(cfg, cfg.Models.Definitions["priced"].Model)
			if err != nil {
				t.Fatalf("RouteModelName: %v", err)
			}
			modelName, err := routeModel(routeName)
			if err != nil {
				t.Fatalf("resolve route: %v", err)
			}

			resolved, err := adkmodel.NewLLM(t.Context(), modelName)
			if err != nil {
				t.Fatalf("resolve %q through the model registry: %v", modelName, err)
			}

			req := &adkmodel.LLMRequest{
				Contents: []*genai.Content{{
					Role:  "user",
					Parts: []*genai.Part{{Text: "hello"}},
				}},
			}

			var invocationErr error
			for _, err := range resolved.GenerateContent(t.Context(), req, false) {
				if err != nil {
					invocationErr = err
					break
				}
			}

			if tc.wantErrCode != "" {
				if invocationErr == nil {
					t.Fatalf("expected error %s, got nil", tc.wantErrCode)
				}
				code, ok := model.CodeOf(invocationErr)
				if !ok || code != tc.wantErrCode {
					t.Fatalf("invocation error code = %v (ok=%v), want %s; err: %v", code, ok, tc.wantErrCode, invocationErr)
				}
			} else if invocationErr != nil {
				t.Fatalf("unexpected invocation error: %v", invocationErr)
			}
			if serverHits == 0 {
				t.Fatal("candidate endpoint was never called")
			}
		})
	}
}
