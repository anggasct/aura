package config

import (
	"fmt"
	"regexp"

	"github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/configkit"
)

const (
	MaxPluginVersionLength = 64
	MaxPluginDesired       = 256
)

var (
	pluginVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*|[0-9]*[a-zA-Z-][0-9a-zA-Z-]*)(\.(0|[1-9][0-9]*|[0-9]*[a-zA-Z-][0-9a-zA-Z-]*))*))?(\+[0-9a-zA-Z-]+(\.[0-9a-zA-Z-]+)*)?$`)
)

type Plugins struct {
	Desired []PluginDesired `json:"desired"`
}

type PluginDesired struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Enabled bool   `json:"enabled"`
}

func ValidPluginVersion(version string) bool {
	return len(version) <= MaxPluginVersionLength && pluginVersionPattern.MatchString(version)
}

func ValidatePlugins(plugins *Plugins) error {
	if plugins == nil {
		return nil
	}
	if len(plugins.Desired) > MaxPluginDesired {
		return configkit.Invalid("plugins.desired", fmt.Sprintf("lists more than %d plugins", MaxPluginDesired))
	}
	seen := make(map[string]struct{}, len(plugins.Desired))
	for i := range plugins.Desired {
		entry := &plugins.Desired[i]
		if !agent.ValidPluginID(entry.ID) {
			return configkit.Invalid(fmt.Sprintf("plugins.desired[%d].id", i), "must match [a-z][a-z0-9-]{1,47}")
		}
		if !ValidPluginVersion(entry.Version) {
			return configkit.Invalid(fmt.Sprintf("plugins.desired[%d].version", i), "must be a semantic version")
		}
		if _, duplicate := seen[entry.ID]; duplicate {
			return configkit.Invalid("plugins.desired", fmt.Sprintf("lists plugin %q twice", entry.ID))
		}
		seen[entry.ID] = struct{}{}
	}
	return nil
}
