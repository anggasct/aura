package model

import (
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/config"
)

func validDefinition(protocol string, caps config.ModelCapabilities) config.ModelDefinition {
	if caps.ContextTokens == 0 {
		caps.ContextTokens = 128000
	}
	if caps.Tokenizer == "" {
		caps.Tokenizer = "cl100k_base"
	}
	return config.ModelDefinition{
		Protocol:     protocol,
		Model:        "test-model",
		Capabilities: caps,
	}
}

func TestValidateRoute_ValidChains(t *testing.T) {
	defs := map[string]config.ModelDefinition{
		"m1": validDefinition(config.ProtocolOpenAIChatCompat, config.ModelCapabilities{Streaming: true, Tools: true}),
		"m2": validDefinition(config.ProtocolAnthropicMessages, config.ModelCapabilities{Streaming: true, Tools: true}),
		"m3": validDefinition(config.ProtocolGeminiNative, config.ModelCapabilities{Streaming: true, Tools: true, Vision: true}),
		"m4": validDefinition(config.ProtocolOpenAIResponses, config.ModelCapabilities{Streaming: true, Tools: true}),
	}

	tests := []struct {
		name       string
		candidates []string
	}{
		{"single", []string{"m1"}},
		{"two-candidates", []string{"m1", "m2"}},
		{"three-candidates", []string{"m1", "m2", "m3"}},
		{"four-candidates", []string{"m1", "m2", "m3", "m4"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			route := config.ModelRoute{
				Candidates:          tt.candidates,
				MaxProviderAttempts: 4,
				RetryDelayBudget:    config.Duration(20 * time.Second),
				CostBudgetUSD:       1.0,
			}
			if err := ValidateRoute("default-route", route, defs, nil); err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestValidateRoute_RejectsInvalidRouteName(t *testing.T) {
	defs := map[string]config.ModelDefinition{
		"m1": validDefinition(config.ProtocolOpenAIChatCompat, config.ModelCapabilities{}),
	}
	invalidNames := []string{"", "123-route", "RouteName", "route/name", "route:name"}
	for _, name := range invalidNames {
		route := config.ModelRoute{Candidates: []string{"m1"}}
		err := ValidateRoute(name, route, defs, nil)
		wantCode(t, err, ErrorCodeRouteInvalid)
	}
}

func TestValidateRoute_RejectsInvalidChainDepth(t *testing.T) {
	defs := map[string]config.ModelDefinition{
		"m1": validDefinition(config.ProtocolOpenAIChatCompat, config.ModelCapabilities{}),
		"m2": validDefinition(config.ProtocolOpenAIChatCompat, config.ModelCapabilities{}),
		"m3": validDefinition(config.ProtocolOpenAIChatCompat, config.ModelCapabilities{}),
		"m4": validDefinition(config.ProtocolOpenAIChatCompat, config.ModelCapabilities{}),
		"m5": validDefinition(config.ProtocolOpenAIChatCompat, config.ModelCapabilities{}),
	}

	// Empty candidates (depth 0)
	err := ValidateRoute("primary", config.ModelRoute{Candidates: nil}, defs, nil)
	wantCode(t, err, ErrorCodeRouteInvalid)

	// Five candidates (depth 5, exceeds max 4)
	err = ValidateRoute("primary", config.ModelRoute{Candidates: []string{"m1", "m2", "m3", "m4", "m5"}}, defs, nil)
	wantCode(t, err, ErrorCodeRouteInvalid)
}

func TestValidateRoute_RejectsDuplicateCandidates(t *testing.T) {
	defs := map[string]config.ModelDefinition{
		"m1": validDefinition(config.ProtocolOpenAIChatCompat, config.ModelCapabilities{}),
		"m2": validDefinition(config.ProtocolOpenAIChatCompat, config.ModelCapabilities{}),
	}

	route := config.ModelRoute{Candidates: []string{"m1", "m2", "m1"}}
	err := ValidateRoute("primary", route, defs, nil)
	wantCode(t, err, ErrorCodeRouteInvalid)
}

func TestValidateRoute_RejectsInlineEndpointsAndSecrets(t *testing.T) {
	defs := map[string]config.ModelDefinition{
		"m1": validDefinition(config.ProtocolOpenAIChatCompat, config.ModelCapabilities{}),
	}

	badCandidates := []string{
		"https://api.openai.com/v1",
		"http://localhost:8000",
		"models/gemini-pro",
		"sk-1234567890abcdef",
		"bearer_token_xyz",
	}

	for _, bad := range badCandidates {
		route := config.ModelRoute{Candidates: []string{bad}}
		err := ValidateRoute("primary", route, defs, nil)
		wantCode(t, err, ErrorCodeRouteInvalid)
	}
}

func TestValidateRoute_RejectsNegativeBounds(t *testing.T) {
	defs := map[string]config.ModelDefinition{
		"m1": validDefinition(config.ProtocolOpenAIChatCompat, config.ModelCapabilities{}),
	}

	routeAttempts := config.ModelRoute{Candidates: []string{"m1"}, MaxProviderAttempts: -1}
	wantCode(t, ValidateRoute("primary", routeAttempts, defs, nil), ErrorCodeRouteInvalid)

	routeDelay := config.ModelRoute{Candidates: []string{"m1"}, RetryDelayBudget: config.Duration(-time.Second)}
	wantCode(t, ValidateRoute("primary", routeDelay, defs, nil), ErrorCodeRouteInvalid)

	routeCost := config.ModelRoute{Candidates: []string{"m1"}, CostBudgetUSD: -0.50}
	wantCode(t, ValidateRoute("primary", routeCost, defs, nil), ErrorCodeRouteInvalid)
}

func TestValidateRoute_RejectsUnknownDefinition(t *testing.T) {
	defs := map[string]config.ModelDefinition{
		"m1": validDefinition(config.ProtocolOpenAIChatCompat, config.ModelCapabilities{}),
	}

	route := config.ModelRoute{Candidates: []string{"unknown-model"}}
	err := ValidateRoute("primary", route, defs, nil)
	wantCode(t, err, ErrorCodeNotFound)
}

func TestValidateRoute_RejectsInvalidCapabilityMetadata(t *testing.T) {
	defsMissingContext := map[string]config.ModelDefinition{
		"m1": {
			Protocol:     config.ProtocolOpenAIChatCompat,
			Model:        "test",
			Capabilities: config.ModelCapabilities{ContextTokens: 0, Tokenizer: "cl100k"},
		},
	}
	wantCode(t, ValidateRoute("primary", config.ModelRoute{Candidates: []string{"m1"}}, defsMissingContext, nil), ErrorCodeCapabilityUnsupported)

	defsMissingTokenizer := map[string]config.ModelDefinition{
		"m2": {
			Protocol:     config.ProtocolOpenAIChatCompat,
			Model:        "test",
			Capabilities: config.ModelCapabilities{ContextTokens: 4096, Tokenizer: ""},
		},
	}
	wantCode(t, ValidateRoute("primary", config.ModelRoute{Candidates: []string{"m2"}}, defsMissingTokenizer, nil), ErrorCodeCapabilityUnsupported)
}

func TestValidateRoute_CapabilityPredicateEnforcement(t *testing.T) {
	defs := map[string]config.ModelDefinition{
		"vision-model": validDefinition(config.ProtocolGeminiNative, config.ModelCapabilities{
			Streaming: true,
			Tools:     true,
			Vision:    true,
		}),
		"text-only-model": validDefinition(config.ProtocolOpenAIChatCompat, config.ModelCapabilities{
			Streaming: true,
			Tools:     true,
			Vision:    false,
		}),
	}

	pred := &CapabilityPredicate{Vision: true}

	// Satisfying candidate succeeds
	err := ValidateRoute("vision-route", config.ModelRoute{Candidates: []string{"vision-model"}}, defs, pred)
	if err != nil {
		t.Fatalf("expected valid route, got %v", err)
	}

	// Missing required capability fails
	err = ValidateRoute("vision-route", config.ModelRoute{Candidates: []string{"text-only-model"}}, defs, pred)
	wantCode(t, err, ErrorCodeCapabilityUnsupported)

	// Chain where second candidate cannot satisfy capability fails
	err = ValidateRoute("vision-route", config.ModelRoute{Candidates: []string{"vision-model", "text-only-model"}}, defs, pred)
	wantCode(t, err, ErrorCodeCapabilityUnsupported)
}

func TestCapabilityPredicate_SatisfiedBy(t *testing.T) {
	caps := config.ModelCapabilities{
		Streaming:        true,
		Tools:            true,
		StructuredOutput: true,
		Vision:           true,
		Audio:            true,
		Reasoning:        true,
		ContextTokens:    64000,
		Tokenizer:        "tiktoken",
		UsageReporting:   true,
	}

	checks := []struct {
		name      string
		pred      CapabilityPredicate
		caps      config.ModelCapabilities
		wantOk    bool
		wantField string
	}{
		{"streaming-missing", CapabilityPredicate{Streaming: true}, config.ModelCapabilities{}, false, "streaming"},
		{"tools-missing", CapabilityPredicate{Tools: true}, config.ModelCapabilities{}, false, "tools"},
		{"structured-missing", CapabilityPredicate{StructuredOutput: true}, config.ModelCapabilities{}, false, "structured_output"},
		{"vision-missing", CapabilityPredicate{Vision: true}, config.ModelCapabilities{}, false, "vision"},
		{"audio-missing", CapabilityPredicate{Audio: true}, config.ModelCapabilities{}, false, "audio"},
		{"reasoning-missing", CapabilityPredicate{Reasoning: true}, config.ModelCapabilities{}, false, "reasoning"},
		{"context-tokens-insufficient", CapabilityPredicate{MinContextTokens: 100000}, caps, false, "context_tokens"},
		{"tokenizer-mismatch", CapabilityPredicate{Tokenizer: "sentencepiece"}, caps, false, "tokenizer"},
		{"usage-reporting-missing", CapabilityPredicate{UsageReporting: true}, config.ModelCapabilities{}, false, "usage_reporting"},
		{"all-satisfied", CapabilityPredicate{Streaming: true, Tools: true, Vision: true}, caps, true, ""},
	}

	for _, tt := range checks {
		t.Run(tt.name, func(t *testing.T) {
			ok, missing := tt.pred.SatisfiedBy(tt.caps)
			if ok != tt.wantOk {
				t.Errorf("SatisfiedBy() ok = %v, want %v", ok, tt.wantOk)
			}
			if missing != tt.wantField {
				t.Errorf("SatisfiedBy() missing = %q, want %q", missing, tt.wantField)
			}
		})
	}
}

func TestPredicateForTask(t *testing.T) {
	visionPred := PredicateForTask("vision")
	if !visionPred.Vision {
		t.Errorf("PredicateForTask(vision) Vision = false, want true")
	}

	defaultPred := PredicateForTask("agent")
	if defaultPred.Vision {
		t.Errorf("PredicateForTask(agent) Vision = true, want false")
	}
}

func TestPredicateForRequest(t *testing.T) {
	if pred := PredicateForRequest(nil); pred != (CapabilityPredicate{}) {
		t.Errorf("PredicateForRequest(nil) = %+v, want empty", pred)
	}

	// Tool definition in request
	reqTools := &adkmodel.LLMRequest{
		Tools: map[string]any{"lookup": struct{}{}},
	}
	if !PredicateForRequest(reqTools).Tools {
		t.Errorf("PredicateForRequest with tools should require Tools")
	}

	// Function call or response in contents
	reqFuncCall := &adkmodel.LLMRequest{
		Contents: []*genai.Content{{
			Parts: []*genai.Part{{
				FunctionCall: &genai.FunctionCall{Name: "lookup"},
			}},
		}},
	}
	if !PredicateForRequest(reqFuncCall).Tools {
		t.Errorf("PredicateForRequest with FunctionCall should require Tools")
	}

	// Image inline data
	reqVision := &adkmodel.LLMRequest{
		Contents: []*genai.Content{{
			Parts: []*genai.Part{{
				InlineData: &genai.Blob{MIMEType: "image/png", Data: []byte("fake")},
			}},
		}},
	}
	if !PredicateForRequest(reqVision).Vision {
		t.Errorf("PredicateForRequest with image should require Vision")
	}

	// Audio inline data
	reqAudio := &adkmodel.LLMRequest{
		Contents: []*genai.Content{{
			Parts: []*genai.Part{{
				InlineData: &genai.Blob{MIMEType: "audio/wav", Data: []byte("fake")},
			}},
		}},
	}
	if !PredicateForRequest(reqAudio).Audio {
		t.Errorf("PredicateForRequest with audio should require Audio")
	}

	// Structured output schema
	reqSchema := &adkmodel.LLMRequest{
		Config: &genai.GenerateContentConfig{
			ResponseSchema: &genai.Schema{Type: genai.TypeObject},
		},
	}
	if !PredicateForRequest(reqSchema).StructuredOutput {
		t.Errorf("PredicateForRequest with schema should require StructuredOutput")
	}

	// JSON response MIME type
	reqJSON := &adkmodel.LLMRequest{
		Config: &genai.GenerateContentConfig{
			ResponseMIMEType: "application/json",
		},
	}
	if !PredicateForRequest(reqJSON).StructuredOutput {
		t.Errorf("PredicateForRequest with json MIME should require StructuredOutput")
	}
}

func TestValidateRoutes_MultiRouteAndTaskCapability(t *testing.T) {
	defs := map[string]config.ModelDefinition{
		"primary-model": validDefinition(config.ProtocolOpenAIChatCompat, config.ModelCapabilities{
			Streaming: true,
			Tools:     true,
		}),
		"aux-model": validDefinition(config.ProtocolAnthropicMessages, config.ModelCapabilities{
			Streaming: true,
			Tools:     true,
			Vision:    true,
		}),
	}

	routes := map[string]config.ModelRoute{
		"primary":   {Candidates: []string{"primary-model"}},
		"auxiliary": {Candidates: []string{"aux-model"}},
	}

	routing := map[string]string{
		"agent":  "primary",
		"vision": "auxiliary",
	}

	if err := ValidateRoutes(defs, routes, routing); err != nil {
		t.Fatalf("unexpected ValidateRoutes error: %v", err)
	}

	// Misrouting vision to a model without vision capability
	badRouting := map[string]string{
		"vision": "primary",
	}
	err := ValidateRoutes(defs, routes, badRouting)
	wantCode(t, err, ErrorCodeCapabilityUnsupported)
}
