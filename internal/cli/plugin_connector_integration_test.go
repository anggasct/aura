//go:build linux && integration

package cli

import (
	"log/slog"
	"os"
	"testing"

	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/sandbox"
)

func TestIntegrationConnectorPluginRunsInsideTheSandboxWithoutANetwork(t *testing.T) {
	script, err := os.ReadFile("testdata/connector-integration-helper.sh")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	f := newConnectorFixtureWith(t, connectorNoAuthManifest, script, nil)
	plan, err := f.service.Plan(t.Context())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	f.runtime = &pluginRuntime{service: f.service, plan: plan, installed: 1}
	f.manager, err = startConnectorPlugins(t.Context(), &connectorRuntimeOptions{
		Broker: f.h.executor.Broker(), Logger: slog.New(slog.DiscardHandler), Starter: newMCPSessionStarter(sandbox.Start),
		HTTPClient: f.api.server.Client(), Plugins: f.runtime.connectorPlugins(),
	})
	if err != nil {
		t.Fatalf("startConnectorPlugins: %v", err)
	}
	t.Cleanup(func() { _ = f.manager.Close(t.Context()) })
	if statuses := f.manager.Statuses(); len(statuses) != 1 || statuses[0].Code != "" || !statuses[0].Connected {
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
	if outputOf(t, env)["home"] != "unset" {
		t.Fatalf("sandboxed connector environment = %s", env)
	}
	if calls := f.api.recorded(); len(calls) != 1 || calls[0].Path != "/v1/messages" {
		t.Fatalf("api saw %+v", calls)
	}
}
