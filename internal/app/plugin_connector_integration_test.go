//go:build linux && integration

package app

import (
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/modules/plugin/connector"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/sandbox"
)

func TestIntegrationConnectorPluginRunsInsideTheSandboxWithoutANetwork(t *testing.T) {
	f := newConnectorFixtureWith(t, connectorNoAuthManifest, goTestConnector(t), nil)
	f.startWithStarter(t, connector.NewSandboxStarter(sandbox.Start))
	if statuses := f.module.ConnectorStatuses(); len(statuses) != 1 || statuses[0].Code != "" || !statuses[0].Connected {
		t.Fatalf("statuses = %+v", statuses)
	}

	output, err := f.call(t, "search", "sandbox-1", principal.ClassOwner, map[string]any{"url": f.api.server.URL + "/v1/messages?q=hi"})
	if err != nil {
		t.Fatalf("search inside the sandbox: %v", err)
	}
	if outputOf(t, output)["status"] != float64(200) {
		t.Fatalf("output = %s", output)
	}
	env, err := f.call(t, "search", "sandbox-env", principal.ClassOwner, map[string]any{"mode": "env"})
	if err != nil {
		t.Fatalf("env probe: %v", err)
	}
	entries, ok := outputOf(t, env)["env"].([]any)
	if !ok {
		t.Fatalf("env probe output = %s", env)
	}
	var seen []string
	for _, entry := range entries {
		text, _ := entry.(string)
		seen = append(seen, text)
	}
	if slices.ContainsFunc(seen, func(entry string) bool { return strings.HasPrefix(entry, "HOME=") }) {
		t.Fatalf("sandboxed connector inherited HOME: %v", seen)
	}
	if !slices.ContainsFunc(seen, func(entry string) bool { return strings.HasPrefix(entry, "TMPDIR=") }) {
		t.Fatalf("sandboxed connector environment = %v, want the scratch directory", seen)
	}
	if calls := f.api.recorded(); len(calls) != 1 || calls[0].Path != "/v1/messages" {
		t.Fatalf("api saw %+v", calls)
	}
}
