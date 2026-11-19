package config

import (
	"strings"
	"testing"
)

func TestPluginIdentifiersRejectPathShapes(t *testing.T) {
	for _, id := range []string{"", "a", "Weather", "we\tather", "../x", "a/b", "a\\b", "weather\n", strings.Repeat("a", 49), "-weather", "1weather", "we_x"} {
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
