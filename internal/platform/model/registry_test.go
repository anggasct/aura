package model

import (
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
)

func TestRegistryResolvesDefinitionsAndRoutes(t *testing.T) {
	t.Setenv("TEST_MODEL_API_KEY", "k")
	models := Config{
		Definitions: map[string]Definition{
			"primary":   configuredDefinition(ProtocolAnthropicMessages, "claude-reg-test-1", "https://api.anthropic.com"),
			"auxiliary": configuredDefinition(ProtocolOpenAIChatCompat, "gpt-reg-test-1", "https://api.openai.com"),
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
	models := Config{
		Definitions: map[string]Definition{
			"primary":   configuredDefinition(ProtocolOpenAIChatCompat, "dup-reg-test-1", "https://api.openai.com"),
			"auxiliary": configuredDefinition(ProtocolAnthropicMessages, "dup-reg-test-1", "https://api.anthropic.com"),
		},
	}
	_, err := NewRegistry(t.Context(), &RegistryOptions{Models: models})
	if err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Errorf("err = %v, want already registered error", err)
	}
}

func TestRegistrySkipsUnconfiguredDefinitions(t *testing.T) {
	if _, err := NewRegistry(t.Context(), &RegistryOptions{Models: Config{}}); err != nil {
		t.Fatalf("NewRegistry(unconfigured): %v", err)
	}
}

func TestRegistriesAreIndependent(t *testing.T) {
	t.Setenv("TEST_MODEL_API_KEY", "k")
	models := Config{
		Definitions: map[string]Definition{
			"primary": configuredDefinition(ProtocolOpenAIChatCompat, "indep-reg-test-1", "https://api.openai.com"),
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
	if !fault.HasCode(err, ErrorCodeNotFound) {
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
	if !fault.HasCode(err, ErrorCodeNotFound) {
		t.Errorf("err = %v, want code %s", err, ErrorCodeNotFound)
	}
}

func TestRegistryCapabilitiesDirectAndFallbackRoutes(t *testing.T) {
	t.Setenv("TEST_MODEL_API_KEY", "k")
	big := configuredDefinition(ProtocolOpenAIChatCompat, "cap-big", "https://api.openai.com")
	big.Capabilities.ContextTokens = 128000
	big.Capabilities.Tokenizer = "o200k_base"
	small := configuredDefinition(ProtocolAnthropicMessages, "cap-small", "https://api.anthropic.com")
	small.Capabilities.ContextTokens = 8192
	small.Capabilities.Tokenizer = "anthropic"
	sameTok := configuredDefinition(ProtocolOpenAIChatCompat, "cap-same", "https://api.openai.com")
	sameTok.Capabilities.ContextTokens = 32768
	sameTok.Capabilities.Tokenizer = "o200k_base"

	models := Config{
		Definitions: map[string]Definition{
			"big":   big,
			"small": small,
			"same":  sameTok,
		},
	}
	routes := map[string]ModelRoute{
		"mixed": {Candidates: []string{"big", "small"}},
		"match": {Candidates: []string{"big", "same"}},
	}
	registry, err := NewRegistry(t.Context(), &RegistryOptions{Models: models, Routes: routes})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	gotBig, ok := registry.Capabilities("cap-big")
	if !ok || gotBig.ContextTokens != 128000 || gotBig.Tokenizer != "o200k_base" {
		t.Fatalf("Capabilities(cap-big) = (%+v, %v)", gotBig, ok)
	}
	gotMixed, ok := registry.Capabilities("mixed")
	if !ok || gotMixed.ContextTokens != 8192 || gotMixed.Tokenizer != "" {
		t.Fatalf("Capabilities(mixed) = (%+v, %v), want min window 8192 and empty tokenizer", gotMixed, ok)
	}
	gotMatch, ok := registry.Capabilities("match")
	if !ok || gotMatch.ContextTokens != 32768 || gotMatch.Tokenizer != "o200k_base" {
		t.Fatalf("Capabilities(match) = (%+v, %v), want min window 32768 and shared tokenizer", gotMatch, ok)
	}
	if _, ok := registry.Capabilities("missing"); ok {
		t.Fatal("Capabilities(missing) = true, want false")
	}
	var nilReg *Registry
	if _, ok := nilReg.Capabilities("cap-big"); ok {
		t.Fatal("nil Registry.Capabilities = true, want false")
	}

	fb, err := registry.LLM(t.Context(), "mixed")
	if err != nil {
		t.Fatalf("LLM(mixed): %v", err)
	}
	adapter, ok := fb.(*FallbackAdapter)
	if !ok {
		t.Fatalf("LLM(mixed) = %T, want *FallbackAdapter", fb)
	}
	fbCaps, ok := adapter.Capabilities()
	if !ok || fbCaps.ContextTokens != 8192 || fbCaps.Tokenizer != "" {
		t.Fatalf("FallbackAdapter.Capabilities() = (%+v, %v)", fbCaps, ok)
	}
}
