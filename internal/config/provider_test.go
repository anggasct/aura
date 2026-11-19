package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestBuiltinProviders_PinnedValues(t *testing.T) {
	builtins := BuiltinProviders()
	if len(builtins) != 5 {
		t.Fatalf("len(BuiltinProviders()) = %d, want 5", len(builtins))
	}
	want := map[string]ProviderPreset{
		"openai":      {Protocol: ProtocolOpenAIResponses, BaseURL: "https://api.openai.com/v1", AuthScheme: ProviderAuthBearer, SessionBehavior: ProviderSessionNone},
		"anthropic":   {Protocol: ProtocolAnthropicMessages, BaseURL: "https://api.anthropic.com", AuthScheme: ProviderAuthBearer, SessionBehavior: ProviderSessionNone},
		"gemini":      {Protocol: ProtocolGeminiNative, BaseURL: "https://generativelanguage.googleapis.com", AuthScheme: ProviderAuthBearer, SessionBehavior: ProviderSessionNone},
		"openrouter":  {Protocol: ProtocolOpenAIChatCompat, BaseURL: "https://openrouter.ai/api/v1", AuthScheme: ProviderAuthBearer, SessionBehavior: ProviderSessionNone},
		"opencode-go": {Protocol: ProtocolOpenAIResponses, BaseURL: "https://opencode.ai/zen/go", AuthScheme: ProviderAuthBearer, SessionBehavior: ProviderSessionOpencode},
	}
	for name, preset := range want {
		got, ok := builtins[name]
		if !ok {
			t.Errorf("missing preset %q", name)
			continue
		}
		if !reflect.DeepEqual(got, preset) {
			t.Errorf("preset %q = %+v, want %+v", name, got, preset)
		}
		if err := ValidateProviderShape("preset "+name, got); err != nil {
			t.Errorf("preset %q shape: %v", name, err)
		}
	}
}

func TestResolveDefinition_LegacyUnchanged(t *testing.T) {
	models := Models{}
	def := ModelDefinition{Protocol: ProtocolAnthropicMessages, BaseURL: "https://api.anthropic.com/"}
	resolved, err := models.ResolveDefinition(&def)
	if err != nil {
		t.Fatalf("ResolveDefinition: %v", err)
	}
	if resolved.Protocol != ProtocolAnthropicMessages || resolved.BaseURL != "https://api.anthropic.com" {
		t.Errorf("resolved = %+v", resolved)
	}
	if resolved.ProviderName != "" || resolved.SessionBehavior != ProviderSessionNone || len(resolved.Headers) != 0 {
		t.Errorf("resolved = %+v, want empty provider fields", resolved)
	}
}

func TestResolveDefinition_OpencodeGoDefaultSingleV1Endpoint(t *testing.T) {
	models := Models{}
	def := ModelDefinition{Provider: "opencode-go", Model: "muse-spark-1.3-contributor"}
	resolved, err := models.ResolveDefinition(&def)
	if err != nil {
		t.Fatalf("ResolveDefinition: %v", err)
	}
	if resolved.BaseURL != "https://opencode.ai/zen/go" {
		t.Fatalf("BaseURL = %q, want %q", resolved.BaseURL, "https://opencode.ai/zen/go")
	}
	endpoint := resolved.BaseURL + "/v1/responses"
	if endpoint != "https://opencode.ai/zen/go/v1/responses" {
		t.Errorf("endpoint = %q, want %q", endpoint, "https://opencode.ai/zen/go/v1/responses")
	}
	if strings.Contains(endpoint, "/v1/v1/") {
		t.Errorf("endpoint %q contains a doubled /v1 path segment", endpoint)
	}
}
