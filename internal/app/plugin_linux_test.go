package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/modules/mcp"
	"github.com/anggasct/aura/internal/modules/plugin"
	"github.com/anggasct/aura/internal/platform/principal"
)

func installApprovedCLIPlugin(t *testing.T, h *mcpHarness, manifest string) *plugin.Service {
	t.Helper()
	service, err := openPluginService(&h.cfg, h.db, discardRedactor{})
	if err != nil {
		t.Fatalf("openPluginService: %v", err)
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

func (h *mcpHarness) startPlugins(t *testing.T, starter mcp.SessionStarter, plugins *plugin.Module) *mcpRuntime {
	t.Helper()
	trustPath, err := mcpTrustRegistryPath(&h.cfg)
	if err != nil {
		t.Fatalf("mcpTrustRegistryPath: %v", err)
	}
	started, err := startMCPRuntime(t.Context(), &mcpRuntimeOptions{
		Config: &h.cfg, Broker: h.executor.Broker(), Logger: slog.New(slog.DiscardHandler), SessionStarter: starter, TrustPath: trustPath, Plugins: mcpPluginsFromBackends(plugins.MCPBackends()),
	})
	if err != nil {
		t.Fatalf("startMCPRuntime: %v", err)
	}
	t.Cleanup(func() { _ = started.Close(context.WithoutCancel(t.Context())) })
	return started
}

func planPluginModule(t *testing.T, h *mcpHarness) *plugin.Module {
	t.Helper()
	dataRoot, err := resolveDataRoot(&h.cfg)
	if err != nil {
		t.Fatalf("resolveDataRoot: %v", err)
	}
	mod, err := plugin.New(t.Context(), &plugin.ModuleDeps{
		Config:   h.cfg.Plugins,
		DB:       h.db,
		DataRoot: dataRoot,
		Redactor: discardRedactor{},
		Logger:   slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("plugin.New: %v", err)
	}
	return mod
}

func TestPluginRuntimeExposesOnlyApprovedToolsOfAnApprovedPlugin(t *testing.T) {
	h := newMCPHarness(t, nil)
	installApprovedCLIPlugin(t, h, cliPluginManifest)
	plugins := planPluginModule(t, h)
	if len(plugins.Plan().Active) != 1 || len(plugins.Plan().Held) != 0 {
		t.Fatalf("plan = %+v", plugins.Plan())
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
	check, ok, err := pluginHealthCheck(plugins, started, time.Duration(h.cfg.Health.CheckTimeout))
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
	plugins := planPluginModule(t, h)
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
	dataRoot, err := resolveDataRoot(&h.cfg)
	if err != nil {
		t.Fatalf("resolveDataRoot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataRoot, "plugins", "demo", "1.0.0", "server"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	plugins := planPluginModule(t, h)
	if len(plugins.Plan().Active) != 0 || len(plugins.Plan().Held) != 1 || plugins.Plan().Held[0].Code != plugin.ErrTampered {
		t.Fatalf("plan = %+v", plugins.Plan())
	}
	if got := mcpPluginsFromBackends(plugins.MCPBackends()); len(got) != 0 {
		t.Fatalf("tampered plugin reached the runtime: %+v", got)
	}
	check, ok, err := pluginHealthCheck(plugins, nil, time.Duration(h.cfg.Health.CheckTimeout))
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
	plugins := planPluginModule(t, h)
	if len(mcpPluginsFromBackends(plugins.MCPBackends())) != 0 {
		t.Fatalf("plugins = %+v", plugins.MCPBackends())
	}
	if _, ok, err := pluginHealthCheck(plugins, nil, time.Duration(h.cfg.Health.CheckTimeout)); err != nil || ok {
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
	service, err := openPluginService(&h.cfg, h.db, discardRedactor{})
	if err != nil {
		t.Fatalf("openPluginService: %v", err)
	}
	installed, err := service.Install(t.Context(), writeCLIPluginBundle(t, cliHTTPPluginManifest))
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	review, err := service.Review(t.Context(), installed.ID)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	text := strings.Join(plugin.FormatReview(review), "\n")
	if strings.Contains(text, "super-secret-value") || !strings.Contains(text, "env://AURA_TEST_PLUGIN_TOKEN") {
		t.Fatalf("review output = %q", text)
	}
}

func TestPluginInstallRejectsAnInlineSecretWithoutEchoingIt(t *testing.T) {
	h := newMCPHarness(t, nil)
	service, err := openPluginService(&h.cfg, h.db, discardRedactor{})
	if err != nil {
		t.Fatalf("openPluginService: %v", err)
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
