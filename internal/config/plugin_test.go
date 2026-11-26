package config

import (
	"strings"
	"testing"
)

func TestValidateMCPServerNamesTheField(t *testing.T) {
	err := ValidateMCPServer(&MCPServer{Transport: MCPTransportStdio, Command: "relative"}, "plugin.backend")
	if err == nil || !strings.Contains(err.Error(), "plugin.backend.command") {
		t.Fatalf("ValidateMCPServer error = %v", err)
	}
}
