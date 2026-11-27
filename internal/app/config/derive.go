package config

import (
	"github.com/anggasct/aura/internal/modules/mcp"
	"github.com/anggasct/aura/internal/modules/search"
	"github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/vision"
)

func deriveDefaults(cfg *Config) {
	deriveEmptyCollections(cfg)
	deriveModelRoutes(cfg)
	deriveWebSearchCredential(cfg.Tools)
	if cfg.MCP != nil {
		for i := range cfg.MCP.Servers {
			s := &cfg.MCP.Servers[i]
			if s.Transport == mcp.TransportStreamableHTTP && s.ConnectTimeout == nil {
				ct := mcp.DefaultConnectTimeout
				s.ConnectTimeout = &ct
			}
		}
	}
}

func deriveEmptyCollections(cfg *Config) {
	if cfg.Models.Providers == nil {
		cfg.Models.Providers = map[string]model.ProviderPreset{}
	}
	if cfg.Vision != nil && cfg.Vision.Providers == nil {
		cfg.Vision.Providers = map[string]vision.ProviderLimits{}
	}
}

func deriveModelRoutes(cfg *Config) {
	if cfg.ModelRoutes == nil && len(cfg.Models.Definitions) > 0 {
		cfg.ModelRoutes = make(map[string]model.ModelRoute)
	}
	for _, name := range []string{"primary", "auxiliary"} {
		if _, defined := cfg.Models.Definitions[name]; !defined {
			continue
		}
		route, exists := cfg.ModelRoutes[name]
		if !exists {
			route = model.DefaultModelRoute()
		}
		if len(route.Candidates) == 0 {
			route.Candidates = []string{name}
		}
		cfg.ModelRoutes[name] = route
	}
}

func deriveWebSearchCredential(tools *Tools) {
	if tools == nil || !tools.WebSearch.CredentialRef.IsZero() {
		return
	}
	tools.WebSearch.CredentialRef = search.DefaultCredentialRef(tools.WebSearch.Provider)
}
