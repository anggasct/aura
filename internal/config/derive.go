package config

func deriveDefaults(cfg *Config) {
	deriveEmptyCollections(cfg)
	deriveModelRoutes(cfg)
	deriveWebSearchCredential(cfg.Tools)
	if cfg.MCP != nil {
		for i := range cfg.MCP.Servers {
			ApplyMCPServerDefaults(&cfg.MCP.Servers[i])
		}
	}
}

func deriveEmptyCollections(cfg *Config) {
	if cfg.Models.Providers == nil {
		cfg.Models.Providers = map[string]ProviderPreset{}
	}
	if cfg.Vision != nil && cfg.Vision.Providers == nil {
		cfg.Vision.Providers = map[string]VisionProviderLimits{}
	}
}

func deriveModelRoutes(cfg *Config) {
	if cfg.ModelRoutes == nil && len(cfg.Models.Definitions) > 0 {
		cfg.ModelRoutes = make(map[string]ModelRoute)
	}
	for _, name := range []string{"primary", "auxiliary"} {
		if _, defined := cfg.Models.Definitions[name]; !defined {
			continue
		}
		route, exists := cfg.ModelRoutes[name]
		if !exists {
			route = defaultModelRoute()
		}
		if len(route.Candidates) == 0 {
			route.Candidates = []string{name}
		}
		cfg.ModelRoutes[name] = route
	}
}

func deriveWebSearchCredential(tools *Tools) {
	if tools == nil || tools.WebSearch.CredentialRef != "" {
		return
	}
	tools.WebSearch.CredentialRef = DefaultWebSearchCredentialRef(tools.WebSearch.Provider)
}
