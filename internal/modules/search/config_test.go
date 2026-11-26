package search

import (
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/secret/secrettest"
)

func TestWebSearchProvidersAreEachAcceptedByValidation(t *testing.T) {
	for _, provider := range Providers() {
		webSearch := Config{Provider: provider, CredentialRef: DefaultCredentialRef(provider), MaxResults: 5}
		if provider == ProviderSearXNG {
			webSearch.Endpoint = "https://search.example.com/search"
		}
		if err := webSearch.Validate(); err != nil {
			t.Errorf("Validate(%s) = %v", provider, err)
		}
	}
}

func TestSearchEndpointOnNilReceiverIsInvalid(t *testing.T) {
	var webSearch *Config
	if _, err := webSearch.SearchEndpoint(); err == nil {
		t.Fatal("SearchEndpoint on nil succeeded")
	}
}

func TestDefaultConfigIsValid(t *testing.T) {
	cfg := Default()
	if cfg.Provider != ProviderDuckDuckGo || !cfg.CredentialRef.IsZero() || cfg.Endpoint != "" || cfg.MaxResults != 5 {
		t.Fatalf("Default() = %+v", cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Default().Validate() = %v", err)
	}
}

func TestConfigValidateNilReceiver(t *testing.T) {
	var cfg *Config
	if err := cfg.Validate(); err != nil {
		t.Fatalf("nil.Validate() = %v", err)
	}
	if secrets := cfg.ConfiguredSecrets(); len(secrets) != 0 {
		t.Fatalf("nil.ConfiguredSecrets() = %v, want empty", secrets)
	}
}

func TestSearchEndpointLoopbackAndDestinationRules(t *testing.T) {
	accepted := map[string]struct {
		endpoint string
		wantURL  string
		loopback bool
	}{
		"loopback ipv4 http": {endpoint: "http://127.0.0.1:8888/search", wantURL: "http://127.0.0.1:8888/search", loopback: true},
		"loopback ipv6 http": {endpoint: "http://[::1]:8888/search", wantURL: "http://[::1]:8888/search", loopback: true},
		"remote https":       {endpoint: "https://search.example.com/search", wantURL: "https://search.example.com:443/search", loopback: false},
	}
	for name, tc := range accepted {
		t.Run("accepts "+name, func(t *testing.T) {
			cfg := Config{Provider: ProviderSearXNG, Endpoint: tc.endpoint, MaxResults: 5}
			ep, err := cfg.SearchEndpoint()
			if err != nil {
				t.Fatalf("SearchEndpoint(%q) = %v", tc.endpoint, err)
			}
			if ep.Loopback != tc.loopback || ep.URL() != tc.wantURL {
				t.Fatalf("SearchEndpoint(%q) = %+v (URL=%q), want loopback=%t URL=%q", tc.endpoint, ep, ep.URL(), tc.loopback, tc.wantURL)
			}
		})
	}

	rejected := map[string]struct {
		endpoint string
		wantText string
	}{
		"https loopback ipv4": {endpoint: "https://127.0.0.1/search", wantText: "must use http for a loopback host"},
		"https loopback ipv6": {endpoint: "https://[::1]:8888/search", wantText: "must use http for a loopback host"},
		"empty path":          {endpoint: "https://search.example.com", wantText: "must carry a non-empty path"},
		"root slash only":     {endpoint: "http://127.0.0.1:8888/", wantText: "must carry a non-empty path"},
		"localhost name":      {endpoint: "http://localhost:8888/search", wantText: "config_invalid"},
		"missing endpoint":    {endpoint: "", wantText: "must be set"},
	}
	for name, tc := range rejected {
		t.Run("rejects "+name, func(t *testing.T) {
			cfg := Config{Provider: ProviderSearXNG, Endpoint: tc.endpoint, MaxResults: 5}
			if _, err := cfg.SearchEndpoint(); err == nil || !fault.HasCode(err, configkit.ErrorCodeInvalid) || !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("SearchEndpoint(%q) = %v, want %q", tc.endpoint, err, tc.wantText)
			}
			err := configkit.Under("tools.web_search", cfg.Validate())
			if err == nil || !fault.HasCode(err, configkit.ErrorCodeInvalid) || !strings.Contains(err.Error(), "tools.web_search.endpoint: ") {
				t.Fatalf("Validate(%q) = %v, want tools.web_search.endpoint error", tc.endpoint, err)
			}
		})
	}
}

func TestConfigValidateRejectsInvalidCombinations(t *testing.T) {
	cases := map[string]struct {
		cfg      Config
		wantPath string
		wantText string
	}{
		"unknown provider": {
			cfg:      Config{Provider: "bing", MaxResults: 5},
			wantPath: "tools.web_search.provider",
			wantText: "must be one of duckduckgo, brave, tavily, searxng",
		},
		"duckduckgo credential": {
			cfg:      Config{Provider: ProviderDuckDuckGo, CredentialRef: secrettest.Ref("env://AURA_TEST_X"), MaxResults: 5},
			wantPath: "tools.web_search.credential_ref",
			wantText: "must be empty for provider duckduckgo",
		},
		"duckduckgo endpoint": {
			cfg:      Config{Provider: ProviderDuckDuckGo, Endpoint: "https://search.example.com/search", MaxResults: 5},
			wantPath: "tools.web_search.endpoint",
			wantText: "is only supported for provider searxng, not duckduckgo",
		},
		"brave missing credential": {
			cfg:      Config{Provider: ProviderBrave, MaxResults: 5},
			wantPath: "tools.web_search.credential_ref",
			wantText: "must use env:// or file:// with a name for provider brave",
		},
		"tavily cross-provider default": {
			cfg:      Config{Provider: ProviderTavily, CredentialRef: DefaultCredentialRef(ProviderBrave), MaxResults: 5},
			wantPath: "tools.web_search.credential_ref",
			wantText: "is the default reference of provider brave and cannot be used with provider tavily",
		},
		"max results zero": {
			cfg:      Config{Provider: ProviderDuckDuckGo, MaxResults: 0},
			wantPath: "tools.web_search.max_results",
			wantText: "must be between 1 and 20",
		},
		"max results above twenty": {
			cfg:      Config{Provider: ProviderDuckDuckGo, MaxResults: 21},
			wantPath: "tools.web_search.max_results",
			wantText: "must be between 1 and 20",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := configkit.Under("tools.web_search", tc.cfg.Validate())
			if err == nil || !fault.HasCode(err, configkit.ErrorCodeInvalid) {
				t.Fatalf("Validate() = %v, want config_invalid", err)
			}
			if !strings.Contains(err.Error(), tc.wantPath+": ") || !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("err = %q, want path %q and detail %q", err.Error(), tc.wantPath, tc.wantText)
			}
		})
	}
}

func TestConfiguredSecretsResolvesCredentialRef(t *testing.T) {
	t.Setenv("AURA_TEST_SEARCH_SECRET_VAL", "secret-val-42")
	cfg := Config{Provider: ProviderBrave, CredentialRef: secrettest.Ref("env://AURA_TEST_SEARCH_SECRET_VAL"), MaxResults: 5}
	if got := cfg.ConfiguredSecrets(); !slices.Equal(got, []string{"secret-val-42"}) {
		t.Fatalf("ConfiguredSecrets() = %v, want [secret-val-42]", got)
	}
	unset := Config{Provider: ProviderBrave, CredentialRef: secrettest.Ref("env://AURA_TEST_SEARCH_SECRET_UNSET"), MaxResults: 5}
	if got := unset.ConfiguredSecrets(); len(got) != 0 {
		t.Fatalf("ConfiguredSecrets(unset) = %v, want empty", got)
	}
}
