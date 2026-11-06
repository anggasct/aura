//go:build linux && integration

package cli

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/sandbox"
)

func TestIntegrationConnectorPluginRunsInsideTheSandboxWithoutANetwork(t *testing.T) {
	f := newConnectorFixtureWith(t, connectorNoAuthManifest, goTestConnector(t), nil)
	plan, err := f.service.Plan(t.Context())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	f.runtime = &pluginRuntime{service: f.service, plan: plan, installed: 1}
	f.manager, err = startConnectorPlugins(t.Context(), &connectorRuntimeOptions{
		Broker: f.h.executor.Broker(), Logger: slog.New(slog.DiscardHandler), Starter: newConnectorSessionStarter(sandbox.Start),
		HTTPClient: f.api.server.Client(), Plugins: f.runtime.connectorPlugins(),
	})
	if err != nil {
		t.Fatalf("startConnectorPlugins: %v", err)
	}
	t.Cleanup(func() { _ = f.manager.Close(context.WithoutCancel(t.Context())) })
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
