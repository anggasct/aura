package config

import (
	"fmt"
	"regexp"

	yamlv3 "gopkg.in/yaml.v3"
)

const (
	MaxPluginVersionLength = 64
	MaxPluginDesired       = 256
)

var (
	pluginIDPattern      = regexp.MustCompile(`^[a-z][a-z0-9-]{1,47}$`)
	pluginVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*|[0-9]*[a-zA-Z-][0-9a-zA-Z-]*)(\.(0|[1-9][0-9]*|[0-9]*[a-zA-Z-][0-9a-zA-Z-]*))*))?(\+[0-9a-zA-Z-]+(\.[0-9a-zA-Z-]+)*)?$`)
)

type Plugins struct {
	Desired []PluginDesired `koanf:"desired" yaml:"desired"`
}

type PluginDesired struct {
	ID      string `koanf:"id" yaml:"id"`
	Version string `koanf:"version" yaml:"version"`
	Enabled bool   `koanf:"enabled" yaml:"enabled"`
}

func ValidPluginID(id string) bool {
	return pluginIDPattern.MatchString(id)
}

func ValidPluginVersion(version string) bool {
	return len(version) <= MaxPluginVersionLength && pluginVersionPattern.MatchString(version)
}

func validatePlugins(plugins *Plugins) error {
	if plugins == nil {
		return nil
	}
	if len(plugins.Desired) > MaxPluginDesired {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: fmt.Sprintf("plugins.desired lists more than %d plugins", MaxPluginDesired)}
	}
	seen := make(map[string]struct{}, len(plugins.Desired))
	for i := range plugins.Desired {
		entry := &plugins.Desired[i]
		if !ValidPluginID(entry.ID) {
			return &Error{Code: ErrorCodeConfigInvalid, Detail: fmt.Sprintf("plugins.desired[%d].id must match [a-z][a-z0-9-]{1,47}", i)}
		}
		if !ValidPluginVersion(entry.Version) {
			return &Error{Code: ErrorCodeConfigInvalid, Detail: fmt.Sprintf("plugins.desired[%d].version must be a semantic version", i)}
		}
		if _, duplicate := seen[entry.ID]; duplicate {
			return &Error{Code: ErrorCodeConfigInvalid, Detail: fmt.Sprintf("plugins.desired lists plugin %q twice", entry.ID)}
		}
		seen[entry.ID] = struct{}{}
	}
	return nil
}

func validatePluginsShapes(doc *yamlv3.Node) error {
	pluginsNode := mappingValue(doc, "plugins")
	if pluginsNode == nil {
		return nil
	}
	if pluginsNode.Kind != yamlv3.MappingNode {
		return fmt.Errorf("plugins must be a mapping at line %d", pluginsNode.Line)
	}
	desiredNode := mappingValue(pluginsNode, "desired")
	if desiredNode == nil {
		return nil
	}
	if desiredNode.Kind != yamlv3.SequenceNode {
		return fmt.Errorf("plugins.desired must be a list at line %d", desiredNode.Line)
	}
	for _, item := range desiredNode.Content {
		if item.Kind != yamlv3.MappingNode {
			return fmt.Errorf("plugins.desired entries must be mappings at line %d", item.Line)
		}
		for i := 0; i+1 < len(item.Content); i += 2 {
			key, value := item.Content[i].Value, item.Content[i+1]
			switch key {
			case "id", "version":
				if value.Kind != yamlv3.ScalarNode || value.Tag != "!!str" {
					return fmt.Errorf("plugins.desired.%s must be a string at line %d", key, value.Line)
				}
			case "enabled":
				if value.Kind != yamlv3.ScalarNode || value.Tag != "!!bool" {
					return fmt.Errorf("plugins.desired.enabled must be a boolean at line %d", value.Line)
				}
			}
		}
	}
	return nil
}

func validatePluginList(field string, plugins []string) error {
	seen := make(map[string]struct{}, len(plugins))
	for _, id := range plugins {
		if !ValidPluginID(id) {
			return &Error{Code: ErrorCodePersonaInvalid, Detail: field + ".plugins entries must be plugin ids"}
		}
		if _, duplicate := seen[id]; duplicate {
			return &Error{Code: ErrorCodePersonaInvalid, Detail: field + ".plugins must not list a plugin twice"}
		}
		seen[id] = struct{}{}
	}
	return nil
}
