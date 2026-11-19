package config

import (
	legacyconfig "github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/vision"
)

func deriveDefaults(cfg *Config) {
	deriveEmptyCollections(cfg)
	deriveModelRoutes(cfg)
	deriveWebSearchCredential(cfg.Tools)
	if cfg.MCP != nil {
		for i := range cfg.MCP.Servers {
			legacyconfig.ApplyMCPServerDefaults(&cfg.MCP.Servers[i])
		}
	}
}

func deriveEmptyCollections(cfg *Config) {
	if cfg.Models.Providers == nil {
		cfg.Models.Providers = map[string]legacyconfig.ProviderPreset{}
	}
	if cfg.Vision != nil && cfg.Vision.Providers == nil {
		cfg.Vision.Providers = map[string]vision.ProviderLimits{}
	}
}

func deriveModelRoutes(cfg *Config) {
	if cfg.ModelRoutes == nil && len(cfg.Models.Definitions) > 0 {
		cfg.ModelRoutes = make(map[string]legacyconfig.ModelRoute)
	}
	for _, name := range []string{"primary", "auxiliary"} {
		if _, defined := cfg.Models.Definitions[name]; !defined {
			continue
		}
		route, exists := cfg.ModelRoutes[name]
		if !exists {
			route = legacyconfig.DefaultModelRoute()
		}
		if len(route.Candidates) == 0 {
			route.Candidates = []string{name}
		}
		cfg.ModelRoutes[name] = route
	}
}

func deriveWebSearchCredential(tools *legacyconfig.Tools) {
	if tools == nil || !tools.WebSearch.CredentialRef.IsZero() {
		return
	}
	tools.WebSearch.CredentialRef = legacyconfig.DefaultWebSearchCredentialRef(tools.WebSearch.Provider)
}
