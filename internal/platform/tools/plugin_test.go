package tools

import (
	"regexp"
	"strings"
	"testing"
)

func TestPluginToolNaming(t *testing.T) {
	if got := PluginToolID("weather", "forecast"); got != "weather.forecast" {
		t.Errorf("PluginToolID = %q", got)
	}
	wire := PluginToolWireName("weather", "forecast")
	if wire != "plugin_weather_forecast" {
		t.Errorf("PluginToolWireName = %q", wire)
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`).MatchString(PluginToolWireName(strings.Repeat("a", 48), "x")) {
		t.Error("wire name is not provider safe")
	}
}

func TestPluginEntryMatching(t *testing.T) {
	cases := []struct {
		name   string
		entry  string
		plugin string
		want   bool
	}{
		{"tool of the plugin", "plugin_weather_forecast", "weather", true},
		{"longer plugin id", "plugin_weather-pro_forecast", "weather", false},
		{"bare prefix", "plugin_weather_", "weather", false},
		{"plugin entry itself", "plugin_weather", "weather", false},
		{"other plugin", "plugin_mail_read", "weather", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NamesPluginTool(tc.entry, tc.plugin); got != tc.want {
				t.Errorf("NamesPluginTool(%q, %q) = %v", tc.entry, tc.plugin, got)
			}
		})
	}
}

func TestListsPluginTool(t *testing.T) {
	entries := []string{"plugin_weather", "plugin_mail_read"}
	if !ListsPluginTool(entries, "plugin_weather_forecast", "weather") {
		t.Error("plugin entry does not cover its tools")
	}
	if !ListsPluginTool(entries, "plugin_mail_read", "mail") {
		t.Error("exact tool entry is not listed")
	}
	if ListsPluginTool(entries, "plugin_mail_send", "mail") {
		t.Error("an unlisted tool of a plugin is listed")
	}
	if ListsPluginTool(entries, "plugin_x_y", "") {
		t.Error("a tool without a plugin matched a plugin entry")
	}
}
