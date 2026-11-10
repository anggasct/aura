package cli

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/modules/mcp"
	"github.com/anggasct/aura/internal/modules/plugin"
	"github.com/anggasct/aura/internal/platform/principal"
)

func installApprovedCLIPlugin(t *testing.T, h *mcpHarness, manifest string) *plugin.Service {
	t.Helper()
	service, err := newPluginService(&h.cfg, h.db, discardRedactor{})
	if err != nil {
		t.Fatalf("newPluginService: %v", err)
	}
	installed, err := service.Install(t.Context(), writeCLIPluginBundle(t, manifest))
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := service.Approve(t.Context(), installed.ID, installed.Digest[:16]); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	return service
}

func (h *mcpHarness) startPlugins(t *testing.T, starter mcp.SessionStarter, plugins *pluginRuntime) *mcpRuntime {
	t.Helper()
	trustPath, err := mcpTrustRegistryPath(&h.cfg)
	if err != nil {
		t.Fatalf("mcpTrustRegistryPath: %v", err)
	}
	started, err := startMCPRuntime(t.Context(), &mcpRuntimeOptions{
		Config: &h.cfg, Broker: h.executor.Broker(), Logger: slog.New(slog.DiscardHandler), SessionStarter: starter, TrustPath: trustPath, Plugins: plugins.mcpPlugins(),
	})
	if err != nil {
		t.Fatalf("startMCPRuntime: %v", err)
	}
	t.Cleanup(func() { _ = started.Close(context.WithoutCancel(t.Context())) })
	return started
}

func TestPluginRuntimeExposesOnlyApprovedToolsOfAnApprovedPlugin(t *testing.T) {
	h := newMCPHarness(t, nil)
	installApprovedCLIPlugin(t, h, cliPluginManifest)
	plugins, err := planPlugins(t.Context(), &h.cfg, h.db, discardRedactor{})
	if err != nil {
		t.Fatalf("planPlugins: %v", err)
	}
	if len(plugins.plan.Active) != 1 || len(plugins.plan.Held) != 0 {
		t.Fatalf("plan = %+v", plugins.plan)
	}
	starter := newFixtureStarter(map[string][]string{"plugin-demo": {"forecast", "export_all"}})
	started := h.startPlugins(t, starter, plugins)
	if started.hasCheck {
		t.Fatal("a plugin-only runtime registered the mcp server check")
	}
	if names := h.toolNames(); !slices.Contains(names, "plugin_demo_forecast") || slices.Contains(names, "plugin_demo_export_all") {
		t.Fatalf("tools = %v", names)
	}
	statuses := started.manager.PluginStatuses()
	if len(statuses) != 1 || !slices.Equal(statuses[0].Drift, []string{"export_all"}) {
		t.Fatalf("statuses = %+v", statuses)
	}
	check, ok, err := plugins.healthCheck(&h.cfg, h.db, started)
	if err != nil || !ok {
		t.Fatalf("healthCheck = %v, %v", ok, err)
	}
	findings := check.Checker.Check(t.Context())
	if codes := findingCodes(findings); codes["demo"] != string(plugin.ErrToolDrift) {
		t.Fatalf("findings = %+v", findings)
	}
}

func TestPluginToolsAreDeniedToPublicAndUnlistedPrincipals(t *testing.T) {
	h := newMCPHarness(t, nil)
	installApprovedCLIPlugin(t, h, cliPluginManifest)
	plugins, err := planPlugins(t.Context(), &h.cfg, h.db, discardRedactor{})
	if err != nil {
		t.Fatalf("planPlugins: %v", err)
	}
	starter := newFixtureStarter(map[string][]string{"plugin-demo": {"forecast"}})
	h.startPlugins(t, starter, plugins)
	if err := h.callTool(t.Context(), "plugin_demo_forecast", "public-call", principal.ClassPublic); err == nil {
		t.Fatal("a public principal called a plugin tool")
	}
	if got := starter.session("plugin-demo").callCount(); got != 0 {
		t.Fatalf("plugin received %d calls from a public principal", got)
	}
}

func TestPlanPluginsKeepsATamperedBundleOff(t *testing.T) {
	h := newMCPHarness(t, nil)
	installApprovedCLIPlugin(t, h, cliPluginManifest)
	dir, err := pluginDirectory(&h.cfg, pluginDirName)
	if err != nil {
		t.Fatalf("pluginDirectory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "demo", "1.0.0", "server"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	plugins, err := planPlugins(t.Context(), &h.cfg, h.db, discardRedactor{})
	if err != nil {
		t.Fatalf("planPlugins: %v", err)
	}
	if len(plugins.plan.Active) != 0 || len(plugins.plan.Held) != 1 || plugins.plan.Held[0].Code != plugin.ErrTampered {
		t.Fatalf("plan = %+v", plugins.plan)
	}
	if got := plugins.mcpPlugins(); len(got) != 0 {
		t.Fatalf("tampered plugin reached the runtime: %+v", got)
	}
	check, ok, err := plugins.healthCheck(&h.cfg, h.db, nil)
	if err != nil || !ok {
		t.Fatalf("healthCheck = %v, %v", ok, err)
	}
	findings := check.Checker.Check(t.Context())
	if codes := findingCodes(findings); codes["demo"] != string(plugin.ErrTampered) {
		t.Fatalf("findings = %+v", findings)
	}
}

func TestPlanPluginsWithoutPluginsIsEmptyAndRegistersNoCheck(t *testing.T) {
	h := newMCPHarness(t, nil)
	plugins, err := planPlugins(t.Context(), &h.cfg, h.db, discardRedactor{})
	if err != nil {
		t.Fatalf("planPlugins: %v", err)
	}
	if len(plugins.mcpPlugins()) != 0 {
		t.Fatalf("plugins = %+v", plugins.mcpPlugins())
	}
	if _, ok, err := plugins.healthCheck(&h.cfg, h.db, nil); err != nil || ok {
		t.Fatalf("healthCheck = %v, %v", ok, err)
	}
}

const cliHTTPPluginManifest = `api_version: aura.plugin/v1
id: remote
version: 1.0.0
name: Remote
description: A remote plugin.
capabilities: []
tools:
  - name: lookup
    description: Look something up.
    tier: read
egress: [api.remote.example]
secrets:
  - name: token
    ref: env://AURA_TEST_PLUGIN_TOKEN
auth:
  kind: static_secret
  secret: token
backend:
  kind: mcp
  transport: streamable_http
  url: https://api.remote.example/mcp
`

func TestPluginReviewShowsSecretReferencesNeverValues(t *testing.T) {
	h := newMCPHarness(t, nil)
	t.Setenv("AURA_TEST_PLUGIN_TOKEN", "super-secret-value")
	service, err := newPluginService(&h.cfg, h.db, discardRedactor{})
	if err != nil {
		t.Fatalf("newPluginService: %v", err)
	}
	installed, err := service.Install(t.Context(), writeCLIPluginBundle(t, cliHTTPPluginManifest))
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	review, err := service.Review(t.Context(), installed.ID)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	text := strings.Join(reviewLines(review), "\n")
	if strings.Contains(text, "super-secret-value") || !strings.Contains(text, "env://AURA_TEST_PLUGIN_TOKEN") {
		t.Fatalf("review output = %q", text)
	}
}

func TestPluginInstallRejectsAnInlineSecretWithoutEchoingIt(t *testing.T) {
	h := newMCPHarness(t, nil)
	service, err := newPluginService(&h.cfg, h.db, discardRedactor{})
	if err != nil {
		t.Fatalf("newPluginService: %v", err)
	}
	manifest := strings.Replace(cliHTTPPluginManifest, "env://AURA_TEST_PLUGIN_TOKEN", "sk-live-inline-value", 1)
	_, err = service.Install(t.Context(), writeCLIPluginBundle(t, manifest))
	if err == nil {
		t.Fatal("an inline secret value was accepted")
	}
	if strings.Contains(err.Error(), "sk-live-inline-value") {
		t.Fatalf("error echoes the secret: %v", err)
	}
}
