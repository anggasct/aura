package model

import (
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/config"
)

func TestRegistryResolvesDefinitionsAndRoutes(t *testing.T) {
	t.Setenv("TEST_MODEL_API_KEY", "k")
	models := config.Models{
		Definitions: map[string]config.ModelDefinition{
			"primary":   configuredDefinition(config.ProtocolAnthropicMessages, "claude-reg-test-1", "https://api.anthropic.com"),
			"auxiliary": configuredDefinition(config.ProtocolOpenAIChatCompat, "gpt-reg-test-1", "https://api.openai.com"),
		},
	}
	registry, err := NewRegistry(t.Context(), &RegistryOptions{Models: models})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	primary, err := registry.LLM(t.Context(), "claude-reg-test-1")
	if err != nil {
		t.Fatalf("LLM(primary): %v", err)
	}
	if _, ok := primary.(*AnthropicAdapter); !ok {
		t.Errorf("primary = %T, want *AnthropicAdapter", primary)
	}

	auxiliary, err := registry.LLM(t.Context(), "gpt-reg-test-1")
	if err != nil {
		t.Fatalf("LLM(auxiliary): %v", err)
	}
	if _, ok := auxiliary.(*OpenAIAdapter); !ok {
		t.Errorf("auxiliary = %T, want *OpenAIAdapter", auxiliary)
	}
}

func TestRegistryRejectsDuplicateNames(t *testing.T) {
	t.Setenv("TEST_MODEL_API_KEY", "k")
	models := config.Models{
		Definitions: map[string]config.ModelDefinition{
			"primary":   configuredDefinition(config.ProtocolOpenAIChatCompat, "dup-reg-test-1", "https://api.openai.com"),
			"auxiliary": configuredDefinition(config.ProtocolAnthropicMessages, "dup-reg-test-1", "https://api.anthropic.com"),
		},
	}
	_, err := NewRegistry(t.Context(), &RegistryOptions{Models: models})
	if err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Errorf("err = %v, want already registered error", err)
	}
}

func TestRegistrySkipsUnconfiguredDefinitions(t *testing.T) {
	if _, err := NewRegistry(t.Context(), &RegistryOptions{Models: config.Models{}}); err != nil {
		t.Fatalf("NewRegistry(unconfigured): %v", err)
	}
}

func TestRegistriesAreIndependent(t *testing.T) {
	t.Setenv("TEST_MODEL_API_KEY", "k")
	models := config.Models{
		Definitions: map[string]config.ModelDefinition{
			"primary": configuredDefinition(config.ProtocolOpenAIChatCompat, "indep-reg-test-1", "https://api.openai.com"),
		},
	}
	for range 2 {
		registry, err := NewRegistry(t.Context(), &RegistryOptions{Models: models})
		if err != nil {
			t.Fatalf("NewRegistry: %v", err)
		}
		if _, err := registry.LLM(t.Context(), "indep-reg-test-1"); err != nil {
			t.Fatalf("LLM: %v", err)
		}
	}
}

func TestRegistryUnknownNameIsNotFound(t *testing.T) {
	registry, err := NewRegistry(t.Context(), &RegistryOptions{})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	_, err = registry.LLM(t.Context(), "missing-reg-test-1")
	if code, ok := CodeOf(err); !ok || code != ErrorCodeNotFound {
		t.Errorf("err = %v, want code %s", err, ErrorCodeNotFound)
	}
	if err == nil || !strings.Contains(err.Error(), "missing-reg-test-1") {
		t.Errorf("err = %v, want it to name the model", err)
	}
}

func TestSetResolvesOnlyItsNames(t *testing.T) {
	known := &stubLLM{name: "set-reg-test-1"}
	set := Set{"set-reg-test-1": known}
	got, err := set.LLM(t.Context(), "set-reg-test-1")
	if err != nil || got != known {
		t.Fatalf("LLM(known) = %v, %v, want the stored instance", got, err)
	}
	_, err = set.LLM(t.Context(), "set-reg-test-other")
	if code, ok := CodeOf(err); !ok || code != ErrorCodeNotFound {
		t.Errorf("err = %v, want code %s", err, ErrorCodeNotFound)
	}
}
