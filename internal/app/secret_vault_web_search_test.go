package app

import (
	"testing"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/modules/search"
	"github.com/anggasct/aura/internal/platform/secret/secrettest"
)

func webSearchSecretEntry(cfg *appconfig.Config) *appconfig.ConfiguredSecret {
	for _, entry := range appconfig.AdditionalConfiguredSecrets(cfg) {
		if entry.Name == "web_search_credential" {
			return &entry
		}
	}
	return nil
}

func TestWebSearchCredentialIsOptionalOnlyAtTheSelectedProvidersDefaultReference(t *testing.T) {
	tests := map[string]struct {
		provider string
		ref      string
		present  bool
		optional bool
	}{
		"brave default":        {provider: "brave", ref: "env://AURA_BRAVE_API_KEY", present: true, optional: true},
		"brave explicit":       {provider: "brave", ref: "env://MY_BRAVE_KEY", present: true, optional: false},
		"tavily default":       {provider: "tavily", ref: "env://AURA_TAVILY_API_KEY", present: true, optional: true},
		"tavily explicit":      {provider: "tavily", ref: "env://MY_TAVILY_KEY", present: true, optional: false},
		"tavily with brave":    {provider: "tavily", ref: "env://AURA_BRAVE_API_KEY", present: true, optional: false},
		"searxng explicit":     {provider: "searxng", ref: "file:///run/secrets/searx", present: true, optional: false},
		"searxng no reference": {provider: "searxng", ref: "", present: false},
		"duckduckgo":           {provider: "duckduckgo", ref: "", present: false},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := &appconfig.Config{Tools: &appconfig.Tools{WebSearch: search.Config{Provider: test.provider, CredentialRef: secrettest.Ref(test.ref)}}}
			entry := webSearchSecretEntry(cfg)
			if (entry != nil) != test.present {
				t.Fatalf("entry = %+v, want present %v", entry, test.present)
			}
			if entry != nil && entry.Optional != test.optional {
				t.Fatalf("optional = %v, want %v", entry.Optional, test.optional)
			}
		})
	}
}
