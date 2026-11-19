package model

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/secret/secrettest"
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
		if err := validateProviderShape("preset "+name, got); err != nil {
			t.Errorf("preset %q shape: %v", name, err)
		}
	}
}

func TestResolveDefinition_LegacyUnchanged(t *testing.T) {
	models := Config{}
	def := Definition{Protocol: ProtocolAnthropicMessages, BaseURL: "https://api.anthropic.com/"}
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
	models := Config{}
	def := Definition{Provider: "opencode-go", Model: "muse-spark-1.3-contributor"}
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

func TestValidateLoadedModelsReportsEveryDefinition(t *testing.T) {
	models := Config{Definitions: map[string]Definition{
		"primary": {Protocol: "nope", Model: "m", APIKeyRef: secrettest.Ref("env://K"),
			Capabilities: Capabilities{ContextTokens: 1, Tokenizer: "t"}},
		"auxiliary": {Protocol: ProtocolOpenAIChatCompat, Model: "",
			Capabilities: Capabilities{ContextTokens: 1, Tokenizer: "t"}},
		"third": {Protocol: ProtocolOpenAIChatCompat, Model: "m", APIKeyRef: secrettest.Ref("env://K"),
			Capabilities: Capabilities{ContextTokens: 0, Tokenizer: ""}},
	}}
	err := models.Validate()
	if err == nil {
		t.Fatal("expected the invalid definitions to be rejected")
	}
	for _, name := range []string{"primary", "auxiliary", "third"} {
		if !strings.Contains(err.Error(), "definitions."+name) {
			t.Errorf("aggregate error omits %q: %v", name, err)
		}
	}
}

func problemTexts(err error) []string {
	if err == nil {
		return nil
	}
	joined, isJoin := err.(interface{ Unwrap() []error })
	if !isJoin {
		return []string{err.Error()}
	}
	var texts []string
	for _, child := range joined.Unwrap() {
		texts = append(texts, child.Error())
	}
	return texts
}

func TestModelConfigValidate(t *testing.T) {
	valid := func(mutate func(*Definition)) Config {
		definition := Definition{
			Protocol:     ProtocolOpenAIChatCompat,
			Model:        "m",
			BaseURL:      "https://x.example.com",
			APIKeyRef:    secrettest.Ref("env://K"),
			Capabilities: Capabilities{ContextTokens: 1, Tokenizer: "t"},
		}
		mutate(&definition)
		return Config{Definitions: map[string]Definition{"primary": definition}}
	}
	cases := []struct {
		name string
		cfg  Config
		code ErrorCode
		want []string
	}{
		{
			name: "non-loopback endpoint without api_key_ref",
			cfg:  valid(func(d *Definition) { d.APIKeyRef = secrettest.Ref("") }),
			code: ErrorCodeSecretInvalid,
			want: []string{"definitions.primary: model_secret_invalid: requires api_key_ref for a non-loopback endpoint"},
		},
		{
			name: "loopback endpoint without api_key_ref",
			cfg: valid(func(d *Definition) {
				d.APIKeyRef = secrettest.Ref("")
				d.BaseURL = "http://127.0.0.1:11434"
			}),
		},
		{
			name: "negative input price",
			cfg:  valid(func(d *Definition) { d.Capabilities.MicrosPerInputToken = -1 }),
			code: ErrorCodeCapabilityUnsupported,
			want: []string{"definitions.primary.capabilities: model_capability_unsupported: cost rates must not be negative"},
		},
		{
			name: "negative output price",
			cfg:  valid(func(d *Definition) { d.Capabilities.MicrosPerOutputToken = -1 }),
			code: ErrorCodeCapabilityUnsupported,
			want: []string{"definitions.primary.capabilities: model_capability_unsupported: cost rates must not be negative"},
		},
		{
			name: "default",
			cfg:  Default(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if got := problemTexts(err); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("problems = %q, want %q", got, tc.want)
			}
			if tc.code != "" && !fault.HasCode(err, tc.code) {
				t.Fatalf("error %v lacks code %s", err, tc.code)
			}
		})
	}

	t.Run("invalid definition name is one key problem", func(t *testing.T) {
		cfg := Config{Definitions: map[string]Definition{"Bad": {Protocol: "nope", BaseURL: "http://example.com"}}}
		err := cfg.Validate()
		problems := problemTexts(err)
		want := []string{"definitions[Bad]: config_invalid: has an invalid name (must match ^[a-z][a-z0-9_-]*$)"}
		if !reflect.DeepEqual(problems, want) {
			t.Fatalf("problems = %q, want %q", problems, want)
		}
		field, isField := errors.AsType[*configkit.FieldError](err)
		if !isField || !field.AtKey {
			t.Fatalf("problem %v is not marked as a key problem", err)
		}
	})
}

func TestModelRoutesValidate(t *testing.T) {
	cases := []struct {
		name   string
		routes Routes
		want   []string
	}{
		{"nil", nil, nil},
		{"empty", Routes{}, nil},
		{"valid", Routes{"main": {Candidates: []string{"a", "b", "c", "d"}, MaxProviderAttempts: 1}}, nil},
		{"invalid name", Routes{"Bad": {Candidates: []string{"a"}}}, []string{`config_invalid: invalid route name "Bad"`}},
		{"no candidates", Routes{"main": {}}, []string{"main: config_invalid: requires at least one candidate"}},
		{"chain too deep", Routes{"main": {Candidates: []string{"a", "b", "c", "d", "e"}}}, []string{"main: config_invalid: exceeds maximum chain depth of 4 candidates"}},
		{"duplicate candidate", Routes{"main": {Candidates: []string{"a", "a"}}}, []string{"main: config_invalid: contains a duplicate candidate"}},
		{"negative attempts", Routes{"main": {Candidates: []string{"a"}, MaxProviderAttempts: -1}}, []string{"main.max_provider_attempts: config_invalid: must not be negative"}},
		{"negative retry budget", Routes{"main": {Candidates: []string{"a"}, RetryDelayBudget: -1}}, []string{"main.retry_delay_budget: config_invalid: must not be negative"}},
		{"negative cost budget", Routes{"main": {Candidates: []string{"a"}, CostBudgetUSD: -0.5}}, []string{"main.cost_budget_usd: config_invalid: must not be negative"}},
		{"negative failure threshold", Routes{"main": {Candidates: []string{"a"}, Circuit: RouteCircuit{FailureThreshold: -1}}}, []string{"main.circuit.failure_threshold: config_invalid: must not be negative"}},
		{"negative open duration", Routes{"main": {Candidates: []string{"a"}, Circuit: RouteCircuit{OpenDuration: -1}}}, []string{"main.circuit.open_duration: config_invalid: must not be negative"}},
		{"negative max open duration", Routes{"main": {Candidates: []string{"a"}, Circuit: RouteCircuit{MaxOpenDuration: -1}}}, []string{"main.circuit.max_open_duration: config_invalid: must not be negative"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := problemTexts(ValidateRoutes(tc.routes)); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("problems = %q, want %q", got, tc.want)
			}
		})
	}
}
