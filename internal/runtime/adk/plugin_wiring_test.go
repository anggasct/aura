package runtimeadk

import (
	"slices"
	"testing"

	"google.golang.org/adk/v2/tool"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/principal"
)

func TestToolsForExposesPluginToolsOnlyToPersonasListingThePlugin(t *testing.T) {
	executor := &ADKExecutor{
		tools: []tool.Tool{
			namedToolStub{name: "read_file"},
			namedToolStub{name: "plugin_weather_forecast"},
			namedToolStub{name: "plugin_weather_alerts"},
			namedToolStub{name: "plugin_mail_read"},
		},
		pluginTools: map[string]string{
			"plugin_weather_forecast": "weather", "plugin_weather_alerts": "weather", "plugin_mail_read": "mail",
		},
	}
	names := func(tools []tool.Tool) []string {
		out := make([]string, 0, len(tools))
		for _, t := range tools {
			out = append(out, t.Name())
		}
		return out
	}
	cases := map[string]struct {
		definition *auraagent.Definition
		class      principal.Class
		want       []string
	}{
		"a persona listing no plugin sees none":            {&auraagent.Definition{}, principal.ClassOwner, []string{"read_file"}},
		"a persona listing no plugin with tools sees none": {&auraagent.Definition{Tools: []string{"read_file"}}, principal.ClassOwner, []string{"read_file"}},
		"owner sees the tools of the listed plugin": {
			&auraagent.Definition{Plugins: []string{"weather"}}, principal.ClassOwner,
			[]string{"read_file", "plugin_weather_forecast", "plugin_weather_alerts"},
		},
		"an unclassified turn sees the listed plugin": {
			&auraagent.Definition{Plugins: []string{"mail"}}, "",
			[]string{"read_file", "plugin_mail_read"},
		},
		"the plugin list does not leak to another plugin": {
			&auraagent.Definition{Plugins: []string{"weather", "mail"}, Tools: []string{"read_file"}}, principal.ClassOwner,
			[]string{"read_file", "plugin_weather_forecast", "plugin_weather_alerts", "plugin_mail_read"},
		},
		"a plugin entry in tools alone is not a listing": {
			&auraagent.Definition{Tools: []string{"read_file", "plugin_weather"}}, principal.ClassOwner,
			[]string{"read_file"},
		},
		"public principals never see plugin tools": {
			&auraagent.Definition{Plugins: []string{"weather"}, Tools: []string{"read_file"}, PublicTools: []string{"read_file"}}, principal.ClassPublic,
			[]string{"read_file"},
		},
		"public principals never see them even when the ceiling names them": {
			&auraagent.Definition{Plugins: []string{"weather"}, PublicTools: []string{"plugin_weather", "plugin_weather_forecast"}}, principal.ClassPublic,
			nil,
		},
		"trusted principals need the ceiling to name the plugin": {
			&auraagent.Definition{Plugins: []string{"weather"}, PrincipalTools: []string{"read_file"}}, principal.ClassTrusted,
			[]string{"read_file"},
		},
		"trusted principals get a plugin named by entry": {
			&auraagent.Definition{Plugins: []string{"weather"}, PrincipalTools: []string{"read_file", "plugin_weather"}}, principal.ClassTrusted,
			[]string{"read_file", "plugin_weather_forecast", "plugin_weather_alerts"},
		},
		"trusted principals get a single named plugin tool": {
			&auraagent.Definition{Plugins: []string{"weather"}, PrincipalTools: []string{"plugin_weather_alerts"}}, principal.ClassTrusted,
			[]string{"plugin_weather_alerts"},
		},
		"the ceiling cannot reach a plugin the persona does not list": {
			&auraagent.Definition{Plugins: []string{"mail"}, PrincipalTools: []string{"plugin_weather"}}, principal.ClassTrusted,
			nil,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := names(executor.toolsFor(tc.definition, tc.class, "", nil))
			if !slices.Equal(got, tc.want) && (len(got) != 0 || len(tc.want) != 0) {
				t.Fatalf("tools = %v, want %v", got, tc.want)
			}
		})
	}
}
