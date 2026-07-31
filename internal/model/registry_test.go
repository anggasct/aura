package model

import (
	"context"
	"strings"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/anggasct/aura/internal/config"
)

func TestRegisterAdapters_ResolvesViaNewLLM(t *testing.T) {
	t.Setenv("TEST_MODEL_API_KEY", "k")
	models := config.Models{
		Definitions: map[string]config.ModelDefinition{
			"primary":   configuredDefinition(config.ProtocolAnthropicMessages, "claude-reg-test-1", "https://api.anthropic.com"),
			"auxiliary": configuredDefinition(config.ProtocolOpenAIChatCompat, "gpt-reg-test-1", "https://api.openai.com"),
		},
	}
	if err := RegisterAdapters(models); err != nil {
		t.Fatalf("RegisterAdapters: %v", err)
	}

	primary, err := adkmodel.NewLLM(context.Background(), "claude-reg-test-1")
	if err != nil {
		t.Fatalf("NewLLM(primary): %v", err)
	}
	if _, ok := primary.(*AnthropicAdapter); !ok {
		t.Errorf("primary = %T, want *AnthropicAdapter", primary)
	}

	auxiliary, err := adkmodel.NewLLM(context.Background(), "gpt-reg-test-1")
	if err != nil {
		t.Fatalf("NewLLM(auxiliary): %v", err)
	}
	if _, ok := auxiliary.(*OpenAIAdapter); !ok {
		t.Errorf("auxiliary = %T, want *OpenAIAdapter", auxiliary)
	}
}

func TestRegisterAdapters_DuplicateModelRejected(t *testing.T) {
	t.Setenv("TEST_MODEL_API_KEY", "k")
	models := config.Models{
		Definitions: map[string]config.ModelDefinition{
			"primary":   configuredDefinition(config.ProtocolOpenAIChatCompat, "dup-reg-test-1", "https://api.openai.com"),
			"auxiliary": configuredDefinition(config.ProtocolAnthropicMessages, "dup-reg-test-1", "https://api.anthropic.com"),
		},
	}
	err := RegisterAdapters(models)
	if err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Errorf("err = %v, want already registered error", err)
	}
}

func TestRegisterAdapters_UnconfiguredNoop(t *testing.T) {
	if err := RegisterAdapters(config.Models{}); err != nil {
		t.Fatalf("RegisterAdapters(unconfigured): %v", err)
	}
}
