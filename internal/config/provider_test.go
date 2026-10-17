package config

import (
	"os"
	"path/filepath"
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
		if err := validateProviderShape("preset "+name, got); err != nil {
			t.Errorf("preset %q shape: %v", name, err)
		}
	}
}

func TestLoad_ProviderReferenceFixture(t *testing.T) {
	res, err := Load(fixture(t, "provider_reference.yaml"))
	if err != nil {
		t.Fatalf("Load(provider_reference): %v", err)
	}
	def := res.Config.Models.Definitions["primary"]
	resolved, err := res.Config.Models.ResolveDefinition(&def)
	if err != nil {
		t.Fatalf("ResolveDefinition: %v", err)
	}
	if resolved.Protocol != ProtocolOpenAIResponses {
		t.Errorf("Protocol = %q, want %q", resolved.Protocol, ProtocolOpenAIResponses)
	}
	if resolved.BaseURL != "https://opencode.ai/zen/go" {
		t.Errorf("BaseURL = %q", resolved.BaseURL)
	}
	if resolved.SessionBehavior != ProviderSessionOpencode {
		t.Errorf("SessionBehavior = %q", resolved.SessionBehavior)
	}
	if resolved.ProviderName != "opencode-go" {
		t.Errorf("ProviderName = %q", resolved.ProviderName)
	}
	if len(resolved.Headers) != 0 {
		t.Errorf("Headers = %v, want empty", resolved.Headers)
	}
}

func TestLoad_CustomProviderFixture(t *testing.T) {
	res, err := Load(fixture(t, "custom_provider.yaml"))
	if err != nil {
		t.Fatalf("Load(custom_provider): %v", err)
	}
	def := res.Config.Models.Definitions["primary"]
	resolved, err := res.Config.Models.ResolveDefinition(&def)
	if err != nil {
		t.Fatalf("ResolveDefinition: %v", err)
	}
	if resolved.Protocol != ProtocolOpenAIChatCompat {
		t.Errorf("Protocol = %q", resolved.Protocol)
	}
	if resolved.BaseURL != "https://models.example.com/v1" {
		t.Errorf("BaseURL = %q", resolved.BaseURL)
	}
	if resolved.Headers["x-tenant"] != "tenant-42" {
		t.Errorf("Headers = %v", resolved.Headers)
	}
	if resolved.ProviderName != "custom" {
		t.Errorf("ProviderName = %q", resolved.ProviderName)
	}
}

func TestLoad_ProviderNegativeFixtures(t *testing.T) {
	for _, name := range []string{"provider_unknown.yaml", "provider_secret_header.yaml", "provider_colliding_header.yaml", "provider_contradictory_protocol.yaml"} {
		_, err := Load(fixture(t, name))
		if err == nil {
			t.Errorf("Load(%s): got nil error, want failure", name)
			continue
		}
		code, ok := CodeOf(err)
		if !ok || code != ErrorCodeModelProviderInvalid {
			t.Errorf("Load(%s): code = %q, %v; want %q", name, code, ok, ErrorCodeModelProviderInvalid)
		}
	}
}

func loadInline(t *testing.T, body string) (Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	res, err := Load(path)
	if err != nil {
		return Config{}, err
	}
	return *res.Config, err
}

const providerTestCapabilities = `      capabilities:
        streaming: true
        tools: false
        structured_output: false
        vision: false
        audio: false
        reasoning: false
        context_tokens: 8000
        tokenizer: test
        usage_reporting: true
`

func TestLoad_ProviderInlineCases(t *testing.T) {
	header := `version: 1
server:
  host: 127.0.0.1
models:
  definitions:
    primary:
`
	tests := []struct {
		name       string
		body       string
		wantCode   ErrorCode
		wantSubstr string
	}{
		{
			name: "provider and custom together",
			body: header + `      provider: openai
      model: m
      api_key_env: K
      custom:
        protocol: openai_chat_compat
        base_url: https://models.example.com/v1
        auth_scheme: bearer
        session_behavior: none
` + providerTestCapabilities,
			wantCode: ErrorCodeModelProviderInvalid,
		},
		{
			name: "custom missing protocol",
			body: header + `      model: m
      api_key_env: K
      custom:
        base_url: https://models.example.com/v1
        auth_scheme: bearer
        session_behavior: none
` + providerTestCapabilities,
			wantCode: ErrorCodeModelProviderInvalid,
		},
		{
			name: "custom missing base_url",
			body: header + `      model: m
      api_key_env: K
      custom:
        protocol: openai_chat_compat
        auth_scheme: bearer
        session_behavior: none
` + providerTestCapabilities,
			wantCode: ErrorCodeModelProviderInvalid,
		},
		{
			name: "custom with top-level protocol",
			body: header + `      protocol: openai_chat_compat
      model: m
      api_key_env: K
      custom:
        protocol: openai_chat_compat
        base_url: https://models.example.com/v1
        auth_scheme: bearer
        session_behavior: none
` + providerTestCapabilities,
			wantCode: ErrorCodeModelProviderInvalid,
		},
		{
			name: "custom invalid auth scheme",
			body: header + `      model: m
      api_key_env: K
      custom:
        protocol: openai_chat_compat
        base_url: https://models.example.com/v1
        auth_scheme: api-key
        session_behavior: none
` + providerTestCapabilities,
			wantCode: ErrorCodeModelProviderInvalid,
		},
		{
			name: "custom invalid session behavior",
			body: header + `      model: m
      api_key_env: K
      custom:
        protocol: openai_chat_compat
        base_url: https://models.example.com/v1
        auth_scheme: bearer
        session_behavior: sticky
` + providerTestCapabilities,
			wantCode: ErrorCodeModelProviderInvalid,
		},
		{
			name: "file secret header",
			body: header + `      model: m
      api_key_env: K
      custom:
        protocol: openai_chat_compat
        base_url: https://models.example.com/v1
        auth_scheme: bearer
        headers:
          x-tenant: file:///run/tenant
        session_behavior: none
` + providerTestCapabilities,
			wantCode: ErrorCodeModelProviderInvalid,
		},
		{
			name: "interpolation secret header",
			body: header + `      model: m
      api_key_env: K
      custom:
        protocol: openai_chat_compat
        base_url: https://models.example.com/v1
        auth_scheme: bearer
        headers:
          x-tenant: ${TENANT_ID}
        session_behavior: none
` + providerTestCapabilities,
			wantCode: ErrorCodeModelProviderInvalid,
		},
		{
			name: "content-type collision case-insensitive",
			body: header + `      model: m
      api_key_env: K
      custom:
        protocol: openai_chat_compat
        base_url: https://models.example.com/v1
        auth_scheme: bearer
        headers:
          Content-Type: application/json
        session_behavior: none
` + providerTestCapabilities,
			wantCode: ErrorCodeModelProviderInvalid,
		},
		{
			name: "session header collision case-insensitive",
			body: header + `      model: m
      api_key_env: K
      custom:
        protocol: openai_chat_compat
        base_url: https://models.example.com/v1
        auth_scheme: bearer
        headers:
          X-OpenCode-Session: fixed
        session_behavior: none
` + providerTestCapabilities,
			wantCode: ErrorCodeModelProviderInvalid,
		},
		{
			name: "unknown key in custom",
			body: header + `      model: m
      api_key_env: K
      custom:
        protocol: openai_chat_compat
        base_url: https://models.example.com/v1
        auth_scheme: bearer
        session_behavior: none
        retry_count: 3
` + providerTestCapabilities,
			wantSubstr: "unknown key",
		},
		{
			name: "non-string provider",
			body: header + `      provider: 42
      model: m
      api_key_env: K
` + providerTestCapabilities,
			wantSubstr: "must be a string",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadInline(t, tc.body)
			if err == nil {
				t.Fatalf("Load: got nil error, want failure")
			}
			if tc.wantCode != "" {
				code, ok := CodeOf(err)
				if !ok || code != tc.wantCode {
					t.Fatalf("code = %q, %v; want %q (err: %v)", code, ok, tc.wantCode, err)
				}
				return
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantSubstr)
			}
		})
	}
}

func TestLoad_ProviderBaseURLOverride(t *testing.T) {
	cfg, err := loadInline(t, `version: 1
server:
  host: 127.0.0.1
models:
  definitions:
    primary:
      provider: openai
      model: gpt-4o-mini
      base_url: https://proxy.example.com/openai
      api_key_env: OPENAI_API_KEY
`+providerTestCapabilities)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	primary := cfg.Models.Definitions["primary"]
	resolved, err := cfg.Models.ResolveDefinition(&primary)
	if err != nil {
		t.Fatalf("ResolveDefinition: %v", err)
	}
	if resolved.BaseURL != "https://proxy.example.com/openai" {
		t.Errorf("BaseURL = %q, want override", resolved.BaseURL)
	}
	if resolved.Protocol != ProtocolOpenAIResponses {
		t.Errorf("Protocol = %q, want preset protocol", resolved.Protocol)
	}
}

func TestLoad_ProvidersOverrideMerges(t *testing.T) {
	cfg, err := loadInline(t, `version: 1
server:
  host: 127.0.0.1
models:
  definitions:
    primary:
      provider: openai
      model: gpt-4o-mini
      api_key_env: OPENAI_API_KEY
`+providerTestCapabilities+`  providers:
    openai:
      protocol: openai_chat_compat
      base_url: https://proxy.example.com/compat
      auth_scheme: bearer
      session_behavior: none
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	primary := cfg.Models.Definitions["primary"]
	resolved, err := cfg.Models.ResolveDefinition(&primary)
	if err != nil {
		t.Fatalf("ResolveDefinition: %v", err)
	}
	if resolved.Protocol != ProtocolOpenAIChatCompat {
		t.Errorf("Protocol = %q, want overridden preset protocol", resolved.Protocol)
	}
	if resolved.BaseURL != "https://proxy.example.com/compat" {
		t.Errorf("BaseURL = %q, want overridden preset base URL", resolved.BaseURL)
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
