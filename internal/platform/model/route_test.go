package model

import (
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/platform/configkit"
)

func validDefinition(protocol string, caps Capabilities) Definition {
	if caps.ContextTokens == 0 {
		caps.ContextTokens = 128000
	}
	if caps.Tokenizer == "" {
		caps.Tokenizer = "cl100k_base"
	}
	return Definition{
		Protocol:     protocol,
		Model:        "test-model",
		Capabilities: caps,
	}
}

func TestValidateRoute_ValidChains(t *testing.T) {
	defs := map[string]Definition{
		"m1": validDefinition(ProtocolOpenAIChatCompat, Capabilities{Streaming: true, Tools: true}),
		"m2": validDefinition(ProtocolAnthropicMessages, Capabilities{Streaming: true, Tools: true}),
		"m3": validDefinition(ProtocolGeminiNative, Capabilities{Streaming: true, Tools: true, Vision: true}),
		"m4": validDefinition(ProtocolOpenAIResponses, Capabilities{Streaming: true, Tools: true}),
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
			route := ModelRoute{
				Candidates:          tt.candidates,
				MaxProviderAttempts: 4,
				RetryDelayBudget:    configkit.Duration(20 * time.Second),
				CostBudgetUSD:       1.0,
			}
			if err := ValidateRoute("default-route", route, defs, nil); err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestValidateRoute_RejectsInvalidRouteName(t *testing.T) {
	defs := map[string]Definition{
		"m1": validDefinition(ProtocolOpenAIChatCompat, Capabilities{}),
	}
	invalidNames := []string{"", "123-route", "RouteName", "route/name", "route:name"}
	for _, name := range invalidNames {
		route := ModelRoute{Candidates: []string{"m1"}}
		err := ValidateRoute(name, route, defs, nil)
		wantCode(t, err, ErrorCodeRouteInvalid)
	}
}

func TestValidateRoute_RejectsInvalidChainDepth(t *testing.T) {
	defs := map[string]Definition{
		"m1": validDefinition(ProtocolOpenAIChatCompat, Capabilities{}),
		"m2": validDefinition(ProtocolOpenAIChatCompat, Capabilities{}),
		"m3": validDefinition(ProtocolOpenAIChatCompat, Capabilities{}),
		"m4": validDefinition(ProtocolOpenAIChatCompat, Capabilities{}),
		"m5": validDefinition(ProtocolOpenAIChatCompat, Capabilities{}),
	}

	err := ValidateRoute("primary", ModelRoute{Candidates: nil}, defs, nil)
	wantCode(t, err, ErrorCodeRouteInvalid)

	err = ValidateRoute("primary", ModelRoute{Candidates: []string{"m1", "m2", "m3", "m4", "m5"}}, defs, nil)
	wantCode(t, err, ErrorCodeRouteInvalid)
}

func TestValidateRoute_RejectsDuplicateCandidates(t *testing.T) {
	defs := map[string]Definition{
		"m1": validDefinition(ProtocolOpenAIChatCompat, Capabilities{}),
		"m2": validDefinition(ProtocolOpenAIChatCompat, Capabilities{}),
	}

	route := ModelRoute{Candidates: []string{"m1", "m2", "m1"}}
	err := ValidateRoute("primary", route, defs, nil)
	wantCode(t, err, ErrorCodeRouteInvalid)
}

func TestValidateRoute_RejectsInlineEndpointsAndSecrets(t *testing.T) {
	defs := map[string]Definition{
		"m1": validDefinition(ProtocolOpenAIChatCompat, Capabilities{}),
	}

	badCandidates := []string{
		"https://api.openai.com/v1",
		"http://localhost:8000",
		"models/gemini-pro",
		"sk-1234567890abcdef",
		"bearer_token_xyz",
	}

	for _, bad := range badCandidates {
		route := ModelRoute{Candidates: []string{bad}}
		err := ValidateRoute("primary", route, defs, nil)
		wantCode(t, err, ErrorCodeRouteInvalid)
	}
}

func TestValidateRoute_RejectsNegativeBounds(t *testing.T) {
	defs := map[string]Definition{
		"m1": validDefinition(ProtocolOpenAIChatCompat, Capabilities{}),
	}

	routeAttempts := ModelRoute{Candidates: []string{"m1"}, MaxProviderAttempts: -1}
	wantCode(t, ValidateRoute("primary", routeAttempts, defs, nil), ErrorCodeRouteInvalid)

	routeDelay := ModelRoute{Candidates: []string{"m1"}, RetryDelayBudget: configkit.Duration(-time.Second)}
	wantCode(t, ValidateRoute("primary", routeDelay, defs, nil), ErrorCodeRouteInvalid)

	routeCost := ModelRoute{Candidates: []string{"m1"}, CostBudgetUSD: -0.50}
	wantCode(t, ValidateRoute("primary", routeCost, defs, nil), ErrorCodeRouteInvalid)
}

func TestValidateRoute_RejectsUnknownDefinition(t *testing.T) {
	defs := map[string]Definition{
		"m1": validDefinition(ProtocolOpenAIChatCompat, Capabilities{}),
	}

	route := ModelRoute{Candidates: []string{"unknown-model"}}
	err := ValidateRoute("primary", route, defs, nil)
	wantCode(t, err, ErrorCodeNotFound)
}

func TestValidateRoute_RejectsInvalidCapabilityMetadata(t *testing.T) {
	defsMissingContext := map[string]Definition{
		"m1": {
			Protocol:     ProtocolOpenAIChatCompat,
			Model:        "test",
			Capabilities: Capabilities{ContextTokens: 0, Tokenizer: "cl100k"},
		},
	}
	wantCode(t, ValidateRoute("primary", ModelRoute{Candidates: []string{"m1"}}, defsMissingContext, nil), ErrorCodeCapabilityUnsupported)

	defsMissingTokenizer := map[string]Definition{
		"m2": {
			Protocol:     ProtocolOpenAIChatCompat,
			Model:        "test",
			Capabilities: Capabilities{ContextTokens: 4096, Tokenizer: ""},
		},
	}
	wantCode(t, ValidateRoute("primary", ModelRoute{Candidates: []string{"m2"}}, defsMissingTokenizer, nil), ErrorCodeCapabilityUnsupported)
}

func TestValidateRoute_CapabilityPredicateEnforcement(t *testing.T) {
	defs := map[string]Definition{
		"vision-model": validDefinition(ProtocolGeminiNative, Capabilities{
			Streaming: true,
			Tools:     true,
			Vision:    true,
		}),
		"text-only-model": validDefinition(ProtocolOpenAIChatCompat, Capabilities{
			Streaming: true,
			Tools:     true,
			Vision:    false,
		}),
	}

	pred := &CapabilityPredicate{Vision: true}

	err := ValidateRoute("vision-route", ModelRoute{Candidates: []string{"vision-model"}}, defs, pred)
	if err != nil {
		t.Fatalf("expected valid route, got %v", err)
	}

	err = ValidateRoute("vision-route", ModelRoute{Candidates: []string{"text-only-model"}}, defs, pred)
	wantCode(t, err, ErrorCodeCapabilityUnsupported)

	err = ValidateRoute("vision-route", ModelRoute{Candidates: []string{"vision-model", "text-only-model"}}, defs, pred)
	wantCode(t, err, ErrorCodeCapabilityUnsupported)
}

func TestCapabilityPredicate_SatisfiedBy(t *testing.T) {
	caps := Capabilities{
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
		caps      Capabilities
		wantOk    bool
		wantField string
	}{
		{"streaming-missing", CapabilityPredicate{Streaming: true}, Capabilities{}, false, "streaming"},
		{"tools-missing", CapabilityPredicate{Tools: true}, Capabilities{}, false, "tools"},
		{"structured-missing", CapabilityPredicate{StructuredOutput: true}, Capabilities{}, false, "structured_output"},
		{"vision-missing", CapabilityPredicate{Vision: true}, Capabilities{}, false, "vision"},
		{"audio-missing", CapabilityPredicate{Audio: true}, Capabilities{}, false, "audio"},
		{"reasoning-missing", CapabilityPredicate{Reasoning: true}, Capabilities{}, false, "reasoning"},
		{"context-tokens-insufficient", CapabilityPredicate{MinContextTokens: 100000}, caps, false, "context_tokens"},
		{"tokenizer-mismatch", CapabilityPredicate{Tokenizer: "sentencepiece"}, caps, false, "tokenizer"},
		{"usage-reporting-missing", CapabilityPredicate{UsageReporting: true}, Capabilities{}, false, "usage_reporting"},
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

	reqTools := &adkmodel.LLMRequest{
		Tools: map[string]any{"lookup": struct{}{}},
	}
	if !PredicateForRequest(reqTools).Tools {
		t.Errorf("PredicateForRequest with tools should require Tools")
	}

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

	reqSchema := &adkmodel.LLMRequest{
		Config: &genai.GenerateContentConfig{
			ResponseSchema: &genai.Schema{Type: genai.TypeObject},
		},
	}
	if !PredicateForRequest(reqSchema).StructuredOutput {
		t.Errorf("PredicateForRequest with schema should require StructuredOutput")
	}

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
	defs := map[string]Definition{
		"primary-model": validDefinition(ProtocolOpenAIChatCompat, Capabilities{
			Streaming: true,
			Tools:     true,
		}),
		"aux-model": validDefinition(ProtocolAnthropicMessages, Capabilities{
			Streaming: true,
			Tools:     true,
			Vision:    true,
		}),
	}

	routes := map[string]ModelRoute{
		"primary":   {Candidates: []string{"primary-model"}},
		"auxiliary": {Candidates: []string{"aux-model"}},
	}

	routing := map[string]string{
		"agent":  "primary",
		"vision": "auxiliary",
	}

	if err := ValidateRouteSet(defs, routes, routing); err != nil {
		t.Fatalf("unexpected ValidateRouteSet error: %v", err)
	}

	badRouting := map[string]string{
		"vision": "primary",
	}
	err := ValidateRouteSet(defs, routes, badRouting)
	wantCode(t, err, ErrorCodeCapabilityUnsupported)
}
