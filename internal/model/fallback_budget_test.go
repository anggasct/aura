package model

import (
	"context"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/usage"
)

func TestFallbackCostBudgetExceeded(t *testing.T) {
	pricedDef := config.ModelDefinition{
		Protocol: config.ProtocolOpenAIChatCompat,
		Model:    "test-model",
		Capabilities: config.ModelCapabilities{
			ContextTokens:        128000,
			Tokenizer:            "cl100k_base",
			Streaming:            true,
			Tools:                true,
			MicrosPerInputToken:  10,  // $0.00001 per input token
			MicrosPerOutputToken: 500, // $0.0005 per output token
		},
	}
	defs := map[string]config.ModelDefinition{"expensive": pricedDef}

	route := config.ModelRoute{
		Candidates:          []string{"expensive"},
		MaxProviderAttempts: 1,
		CostBudgetUSD:       0.001, // 1,000 micros
	}

	prices := usage.NewPriceRegistry()
	if err := prices.Put(&usage.Price{
		ModelDefinitionID:    "expensive",
		Currency:             "USD",
		MicrosPerInputToken:  10,
		MicrosPerOutputToken: 500,
		MaxReservationRate:   100,
		EffectiveFrom:        time.Unix(0, 0).UTC(),
	}); err != nil {
		t.Fatalf("register price: %v", err)
	}

	expensive := &mockCandidateLLM{
		name: "expensive",
		responses: []*adkmodel.LLMResponse{
			{
				Content:      &genai.Content{Parts: []*genai.Part{{Text: "costly answer"}}},
				TurnComplete: true,
				UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
					PromptTokenCount:     2,
					CandidatesTokenCount: 4,
				},
			},
		},
	}

	fallback := NewFallbackAdapter("primary-route", route, defs, nil, MapAdapterResolver{"expensive": expensive}).WithPrices(prices)

	req := &adkmodel.LLMRequest{
		Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hello"}}}},
	}
	var gotErr error
	for _, err := range fallback.GenerateContent(context.Background(), req, false) {
		if err != nil {
			gotErr = err
		}
	}
	if gotErr == nil {
		t.Fatal("invocation crossing cost_budget_usd succeeded; want model_budget_exceeded")
	}
	if code, ok := CodeOf(gotErr); !ok || code != ErrorCodeBudgetExceeded {
		t.Fatalf("error code = %q (found=%v), want %q", code, ok, ErrorCodeBudgetExceeded)
	}

	cheap := &mockCandidateLLM{
		name: "expensive",
		responses: []*adkmodel.LLMResponse{
			{
				Content:      &genai.Content{Parts: []*genai.Part{{Text: "cheap answer"}}},
				TurnComplete: true,
				UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
					PromptTokenCount:     1,
					CandidatesTokenCount: 1,
				},
			},
		},
	}
	underCap := NewFallbackAdapter("primary-route", route, defs, nil, MapAdapterResolver{"expensive": cheap}).WithPrices(prices)
	for _, err := range underCap.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatalf("invocation under the cost cap failed: %v", err)
		}
	}
}
