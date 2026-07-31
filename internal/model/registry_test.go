package model

import (
	"context"
	"strings"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/anggasct/aura/internal/config"
)

func TestRegisterAdapters_ResolvesViaNewLLM(t *testing.T) {
	models := config.Models{
		Primary:   config.ModelSpec{Provider: "anthropic", Model: "claude-reg-test-1", APIKey: "k"},
		Auxiliary: config.ModelSpec{Provider: "openai", Model: "gpt-reg-test-1", APIKey: "k"},
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
	models := config.Models{
		Primary:   config.ModelSpec{Provider: "openai", Model: "dup-reg-test-1", APIKey: "k"},
		Auxiliary: config.ModelSpec{Provider: "anthropic", Model: "dup-reg-test-1", APIKey: "k"},
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
