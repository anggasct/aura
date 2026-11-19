package config

import (
	"errors"
	"fmt"
	"slices"

	"github.com/anggasct/aura/internal/platform/capability"
	"github.com/anggasct/aura/internal/platform/configkit"
)

func validateAgainstBuild(cfg *Config, profile capability.Profile) error {
	if profile == capability.ProfileCore {
		return nil
	}
	var problems []error
	for _, section := range []struct {
		name    string
		missing bool
	}{
		{"tools", cfg.Tools == nil},
		{"skills", cfg.Skills == nil},
		{"context", cfg.Context == nil},
		{"profile", cfg.Profile == nil},
		{"children", cfg.Children == nil},
		{"vision", cfg.Vision == nil},
	} {
		if section.missing {
			problems = append(problems, configkit.Invalid(section.name, fmt.Sprintf("section is required for build profile %q", profile)))
		}
	}
	return errors.Join(problems...)
}

func validateGitHubCredentials(cfg *Config, active []string) error {
	if cfg.Tools != nil && len(cfg.Tools.GitHub.Credentials) > 0 && !slices.Contains(active, githubCapability) {
		return configkit.Invalid("tools.github.credentials", "needs the "+githubCapability+" capability: compile it into the build and list it in capabilities.enabled")
	}
	return nil
}
