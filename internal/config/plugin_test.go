package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePluginsConfig(t *testing.T, plugins string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("version: 1\nplugins:\n"+plugins), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoad_PluginsDesiredValidation(t *testing.T) {
	cases := map[string]struct {
		body   string
		detail string
	}{
		"valid":              {body: "  desired:\n    - id: weather\n      version: 1.2.3\n      enabled: true\n    - id: mail-reader\n      version: 0.1.0-rc.1\n      enabled: false\n"},
		"empty":              {body: "  desired: []\n"},
		"id uppercase":       {body: "  desired:\n    - id: Weather\n      version: 1.0.0\n      enabled: true\n", detail: "plugins.desired[0].id"},
		"id traversal":       {body: "  desired:\n    - id: ../etc\n      version: 1.0.0\n      enabled: true\n", detail: "plugins.desired[0].id"},
		"id too short":       {body: "  desired:\n    - id: a\n      version: 1.0.0\n      enabled: true\n", detail: "plugins.desired[0].id"},
		"version not semver": {body: "  desired:\n    - id: weather\n      version: \"1.0\"\n      enabled: true\n", detail: "plugins.desired[0].version"},
		"version traversal":  {body: "  desired:\n    - id: weather\n      version: ../1.0.0\n      enabled: true\n", detail: "plugins.desired[0].version"},
		"duplicate id":       {body: "  desired:\n    - id: weather\n      version: 1.0.0\n      enabled: true\n    - id: weather\n      version: 1.1.0\n      enabled: false\n", detail: "twice"},
		"enabled not bool":   {body: "  desired:\n    - id: weather\n      version: 1.0.0\n      enabled: \"yes\"\n", detail: "enabled"},
		"version not string": {body: "  desired:\n    - id: weather\n      version: 1\n      enabled: true\n", detail: "version"},
		"unknown key":        {body: "  desired:\n    - id: weather\n      version: 1.0.0\n      enabled: true\n      pin: true\n", detail: "pin"},
		"unknown section":    {body: "  installed: []\n", detail: "installed"},
		"not a list":         {body: "  desired: weather\n", detail: "plugins.desired"},
	}
	for label, tc := range cases {
		t.Run(label, func(t *testing.T) {
			_, err := Load(writePluginsConfig(t, tc.body))
			if tc.detail == "" {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.detail) {
				t.Fatalf("Load error = %v, want mention of %q", err, tc.detail)
			}
		})
	}
}

func TestLoad_PluginsDesiredIsParsed(t *testing.T) {
	result, err := Load(writePluginsConfig(t, "  desired:\n    - id: weather\n      version: 1.2.3\n      enabled: true\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := PluginDesired{ID: "weather", Version: "1.2.3", Enabled: true}
	if result.Config.Plugins == nil || len(result.Config.Plugins.Desired) != 1 || result.Config.Plugins.Desired[0] != want {
		t.Fatalf("Plugins = %+v, want %+v", result.Config.Plugins, want)
	}
}

func TestPluginIdentifiersRejectPathShapes(t *testing.T) {
	for _, id := range []string{"", "a", "Weather", "wea ther", "../x", "a/b", "a\\b", "weather\n", strings.Repeat("a", 49), "-weather", "1weather", "wea_ther"} {
		if ValidPluginID(id) {
			t.Errorf("ValidPluginID(%q) = true", id)
		}
	}
	for _, id := range []string{"ab", "weather", "mail-reader", strings.Repeat("a", 48)} {
		if !ValidPluginID(id) {
			t.Errorf("ValidPluginID(%q) = false", id)
		}
	}
	for _, version := range []string{"", "1", "1.0", "01.0.0", "1.0.0.0", "../1.0.0", "1.0.0/x", "v1.0.0", "1.0.0-", "1.0.0 ", strings.Repeat("1", 65)} {
		if ValidPluginVersion(version) {
			t.Errorf("ValidPluginVersion(%q) = true", version)
		}
	}
	for _, version := range []string{"0.0.0", "1.2.3", "10.20.30", "1.0.0-alpha.1", "1.0.0-rc.1+build.5", "1.0.0+20260101"} {
		if !ValidPluginVersion(version) {
			t.Errorf("ValidPluginVersion(%q) = false", version)
		}
	}
}

func TestValidateMCPServerNamesTheField(t *testing.T) {
	err := ValidateMCPServer(&MCPServer{Transport: MCPTransportStdio, Command: "relative"}, "plugin.backend")
	if err == nil || !strings.Contains(err.Error(), "plugin.backend.command") {
		t.Fatalf("ValidateMCPServer error = %v", err)
	}
}
