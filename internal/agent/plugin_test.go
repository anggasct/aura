package agent

import (
	"slices"
	"testing"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
)

func pluginPersona(plugins, principals, public []string) config.AgentDefinition {
	override := kantorOverride()
	override.Plugins = plugins
	override.PrincipalTools = principals
	override.PublicTools = public
	return override
}

func TestBuildCarriesPersonaPlugins(t *testing.T) {
	registry, err := Build([]config.AgentDefinition{pluginPersona([]string{"weather", "mail-reader"}, nil, nil)}, validTools, []string{"primary"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	definition, ok := registry.Lookup("kantor")
	if !ok || !slices.Equal(definition.Plugins, []string{"weather", "mail-reader"}) {
		t.Fatalf("plugins = %v", definition.Plugins)
	}
	if !definition.ListsPlugin("weather") || definition.ListsPlugin("other") {
		t.Fatal("ListsPlugin does not follow the persona allowlist")
	}
	definition.Plugins[0] = "changed"
	again, _ := registry.Lookup("kantor")
	if again.Plugins[0] != "weather" {
		t.Fatal("Lookup shares the plugin slice with the registry")
	}
}

func TestBuildValidatesPluginToolCeilings(t *testing.T) {
	cases := []struct {
		name     string
		override config.AgentDefinition
		ok       bool
	}{
		{"principal plugin entry for a listed plugin", pluginPersona([]string{"weather"}, []string{"plugin_weather"}, nil), true},
		{"principal plugin tool entry for a listed plugin", pluginPersona([]string{"weather"}, []string{"plugin_weather_forecast"}, nil), true},
		{"principal plugin entry for an unlisted plugin", pluginPersona([]string{"weather"}, []string{"plugin_mail"}, nil), false},
		{"principal plugin entry with no plugins", pluginPersona(nil, []string{"plugin_weather"}, nil), false},
		{"principal entry for a longer plugin id", pluginPersona([]string{"weather"}, []string{"plugin_weather-pro_forecast"}, nil), false},
		{"principal bare prefix", pluginPersona([]string{"weather"}, []string{"plugin_weather_"}, nil), false},
		{"public plugin entry", pluginPersona([]string{"weather"}, nil, []string{"plugin_weather"}), false},
		{"public plugin tool entry", pluginPersona([]string{"weather"}, nil, []string{"plugin_weather_forecast"}), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Build([]config.AgentDefinition{tc.override}, validTools, []string{"primary"})
			if tc.ok {
				if err != nil {
					t.Fatalf("Build: %v", err)
				}
				return
			}
			if code, ok := CodeOf(err); !ok || code != ErrorCodePersonaInvalid {
				t.Fatalf("Build code = %q, %v (err %v), want persona_invalid", code, ok, err)
			}
		})
	}
}

func TestPublicCeilingNeverContainsPluginTools(t *testing.T) {
	registry, err := Build([]config.AgentDefinition{pluginPersona([]string{"weather"}, []string{"plugin_weather"}, nil)}, validTools, []string{"primary"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	definition, _ := registry.Lookup("kantor")
	ceiling, restricted := definition.ToolCeiling(principal.ClassPublic)
	if !restricted || len(ceiling) != 0 {
		t.Fatalf("public ceiling = %v restricted %v", ceiling, restricted)
	}
}

func TestPluginsChangeThePersonaFingerprintAndEvalDigest(t *testing.T) {
	base := Definition{ID: "ops", Tools: []string{"read_file"}}
	withPlugins := Definition{ID: "ops", Tools: []string{"read_file"}, Plugins: []string{"weather"}}
	reordered := Definition{ID: "ops", Tools: []string{"read_file"}, Plugins: []string{"weather", "mail-reader"}}
	swapped := Definition{ID: "ops", Tools: []string{"read_file"}, Plugins: []string{"mail-reader", "weather"}}
	if base.Fingerprint().ToolsSHA256 == withPlugins.Fingerprint().ToolsSHA256 {
		t.Error("fingerprint ignores plugins")
	}
	if base.EvalDigest() == withPlugins.EvalDigest() {
		t.Error("eval digest ignores plugins")
	}
	if reordered.Fingerprint() != swapped.Fingerprint() || reordered.EvalDigest() != swapped.EvalDigest() {
		t.Error("plugin order changes the fingerprint")
	}
	empty := Definition{ID: "ops", Tools: []string{"read_file"}, Plugins: []string{}}
	if empty.Fingerprint() != base.Fingerprint() || empty.EvalDigest() != base.EvalDigest() {
		t.Error("an empty plugin list changes the fingerprint")
	}
}
