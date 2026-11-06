package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/effect"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/store/storetest"
	"github.com/anggasct/aura/internal/toolbroker"
	"github.com/anggasct/aura/internal/tools"
)

func countingBroker(t *testing.T, prompts *int) *toolbroker.Broker {
	t.Helper()
	db := storetest.OpenMigrated(t)
	session := store.Session{ID: "session-1", OwnerID: "principal-1"}
	if err := store.NewSessionService(db).Create(t.Context(), &session); err != nil {
		t.Fatalf("Create(): %v", err)
	}
	journal, err := effect.NewJournal(db, effect.Options{})
	if err != nil {
		t.Fatalf("NewJournal(): %v", err)
	}
	effects, err := effect.NewExecutor(journal)
	if err != nil {
		t.Fatalf("NewExecutor(): %v", err)
	}
	broker, err := toolbroker.New(&toolbroker.Options{
		Effects: effects,
		ApprovalDecider: func(context.Context, *toolbroker.ApprovalPrompt) (bool, error) {
			*prompts++
			return true, nil
		},
	})
	if err != nil {
		t.Fatalf("toolbroker.New(): %v", err)
	}
	return broker
}

func pluginServer(id string, approved ...string) PluginServer {
	cfg := startableStdioConfig(id)
	cfg.Name = id
	cfg.Capabilities = []string{"network"}
	plugin := PluginServer{ID: id, Server: cfg}
	for _, name := range approved {
		plugin.Tools = append(plugin.Tools, PluginTool{Name: name, Description: "approved text for " + name})
	}
	return plugin
}

func startPluginManager(t *testing.T, broker *toolbroker.Broker, servers []config.MCPServer, starter SessionStarter, plugins ...PluginServer) *Manager {
	t.Helper()
	mgr, err := NewManager(&ManagerOptions{
		Config:         &config.MCP{Servers: servers},
		Broker:         broker,
		TrustRegistry:  NewMemoryTrustRegistry(),
		SessionStarter: starter,
		Plugins:        plugins,
	})
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close(context.WithoutCancel(t.Context())) })
	return mgr
}

func pluginToolNames(broker *toolbroker.Broker) []string {
	var names []string
	listed := broker.Definitions()
	for i := range listed {
		def := &listed[i]
		if def.Plugin != nil {
			names = append(names, def.Name)
		}
	}
	return names
}

func toolDefinition(broker *toolbroker.Broker, name string) (tools.Definition, bool) {
	listed := broker.Definitions()
	for i := range listed {
		def := &listed[i]
		if def.Name == name {
			return *def, true
		}
	}
	return tools.Definition{}, false
}

func TestManagerRunsAnApprovedPluginWithoutTheTrustRegistry(t *testing.T) {
	var prompts int
	broker := countingBroker(t, &prompts)
	starter := &stubSessionStarter{session: &scriptedSession{toolNames: []string{"forecast", "export_all"}}}
	mgr := startPluginManager(t, broker, nil, starter, pluginServer("weather", "forecast"))
	if err := mgr.Start(t.Context()); err != nil {
		t.Fatalf("Start(): %v", err)
	}
	def, ok := toolDefinition(broker, "plugin_weather_forecast")
	if !ok {
		t.Fatal("the approved tool is not registered")
	}
	if def.Plugin == nil || def.Plugin.ID != "weather" || def.Plugin.Description != "approved text for forecast" || def.Server != "" {
		t.Fatalf("definition = %+v", def)
	}
	if !def.Effectful || !def.RequiresApproval {
		t.Fatalf("definition = effectful %v approval %v", def.Effectful, def.RequiresApproval)
	}
	if names := pluginToolNames(broker); !slices.Equal(names, []string{"plugin_weather_forecast"}) {
		t.Fatalf("plugin tools = %v", names)
	}
	if brokerHasTool(broker, "mcp_weather_forecast") {
		t.Fatal("a plugin tool leaked as an mcp tool")
	}
	statuses := mgr.PluginStatuses()
	if len(statuses) != 1 || statuses[0].ID != "weather" || statuses[0].Code != "" || !statuses[0].Connected {
		t.Fatalf("statuses = %+v", statuses)
	}
	if !slices.Equal(statuses[0].Tools, []string{"plugin_weather_forecast"}) || !slices.Equal(statuses[0].Drift, []string{"export_all"}) {
		t.Fatalf("status tools/drift = %+v", statuses[0])
	}
	if got := mgr.Statuses(); len(got) != 0 {
		t.Fatalf("plugin leaked into the server statuses: %+v", got)
	}
}

func TestManagerKeepsAUndeclaredPluginToolHiddenFromTheBroker(t *testing.T) {
	var prompts int
	broker := countingBroker(t, &prompts)
	starter := &stubSessionStarter{session: &scriptedSession{toolNames: []string{"export_all", "wipe"}}}
	mgr := startPluginManager(t, broker, nil, starter, pluginServer("weather", "forecast"))
	if err := mgr.Start(t.Context()); err != nil {
		t.Fatalf("Start(): %v", err)
	}
	if names := pluginToolNames(broker); len(names) != 0 {
		t.Fatalf("plugin tools = %v, want none", names)
	}
	if brokerHasTool(broker, "mcp_weather_export_all") || brokerHasTool(broker, "mcp_plugin-weather_export_all") {
		t.Fatal("an undeclared tool leaked as an mcp tool")
	}
	if status := mgr.PluginStatuses()[0]; !slices.Equal(status.Drift, []string{"export_all", "wipe"}) || len(status.Tools) != 0 {
		t.Fatalf("status = %+v", status)
	}
}

func TestPluginToolsNeverLoosenBrokerPolicy(t *testing.T) {
	var prompts int
	broker := countingBroker(t, &prompts)
	starter := &stubSessionStarter{session: &scriptedSession{toolNames: []string{"forecast"}}}
	mgr := startPluginManager(t, broker, nil, starter, pluginServer("weather", "forecast"))
	if err := mgr.Start(t.Context()); err != nil {
		t.Fatalf("Start(): %v", err)
	}
	owner := withEffectIdentity(&toolbroker.ToolRequest{
		RequestID: "r1", TurnID: "t1", SessionID: "session-1", PrincipalID: "principal-1",
		ToolName: "plugin_weather_forecast", ToolVersion: "v1", Arguments: json.RawMessage(`{}`),
		Capabilities: []string{"network"}, Trust: approval.TrustOwnerInput,
	}, "plugin-owner-1")
	if _, err := broker.Execute(t.Context(), owner); err != nil {
		t.Fatalf("owner call: %v", err)
	}
	if prompts != 1 {
		t.Fatalf("approval prompts = %d, want the owner asked once", prompts)
	}
	for _, class := range []string{"trusted", "public"} {
		request := withEffectIdentity(&toolbroker.ToolRequest{
			RequestID: "r-" + class, TurnID: "t1", SessionID: "session-1", PrincipalID: "principal-1",
			ToolName: "plugin_weather_forecast", ToolVersion: "v1", Arguments: json.RawMessage(`{}`),
			Capabilities: []string{"network"}, Trust: approval.TrustOwnerInput,
		}, "plugin-"+class)
		request.PrincipalClass = "trusted"
		if class == "public" {
			request.PrincipalClass = "public"
		}
		if _, err := broker.Execute(t.Context(), request); err == nil {
			t.Fatalf("a %s principal executed a plugin tool", class)
		}
	}
	if prompts != 1 {
		t.Fatalf("approval prompts = %d after refused principals, want 1", prompts)
	}
}

func TestPluginAndServerWithTheSameNameCoexist(t *testing.T) {
	var prompts int
	broker := countingBroker(t, &prompts)
	cfg := startableStdioConfig("weather")
	registry := NewMemoryTrustRegistry()
	approveConfigDigest(t.Context(), t, registry, &cfg)
	starter := &perServerStarter{sessions: map[string]ContainedSession{
		"weather":        &scriptedSession{toolNames: []string{"forecast"}},
		"plugin-weather": &scriptedSession{toolNames: []string{"forecast"}},
	}}
	mgr, err := NewManager(&ManagerOptions{
		Config:         &config.MCP{Servers: []config.MCPServer{cfg}},
		Broker:         broker,
		TrustRegistry:  registry,
		SessionStarter: starter,
		Plugins:        []PluginServer{pluginServer("weather", "forecast")},
	})
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close(context.WithoutCancel(t.Context())) })
	if err := mgr.Start(t.Context()); err != nil {
		t.Fatalf("Start(): %v", err)
	}
	if !brokerHasTool(broker, "mcp_weather_forecast") || !brokerHasTool(broker, "plugin_weather_forecast") {
		t.Fatalf("definitions = %v", broker.Definitions())
	}
	mcpDef, _ := toolDefinition(broker, "mcp_weather_forecast")
	if mcpDef.Plugin != nil || mcpDef.Server != "weather" {
		t.Fatalf("server tool definition = %+v", mcpDef)
	}
	if starter.started("weather") != 1 || starter.started("plugin-weather") != 1 {
		t.Fatalf("starts = %d/%d, want one each", starter.started("weather"), starter.started("plugin-weather"))
	}
}

func TestManagerCloseUnregistersPluginTools(t *testing.T) {
	var prompts int
	broker := countingBroker(t, &prompts)
	starter := &stubSessionStarter{session: &scriptedSession{toolNames: []string{"forecast"}}}
	mgr := startPluginManager(t, broker, nil, starter, pluginServer("weather", "forecast"))
	if err := mgr.Start(t.Context()); err != nil {
		t.Fatalf("Start(): %v", err)
	}
	if err := mgr.Close(t.Context()); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	if brokerHasTool(broker, "plugin_weather_forecast") {
		t.Fatal("plugin tool survived Close")
	}
}

func TestManagerReportsAPluginThatFailsToStart(t *testing.T) {
	var prompts int
	broker := countingBroker(t, &prompts)
	starter := &stubSessionStarter{err: errors.New("no sandbox")}
	mgr := startPluginManager(t, broker, nil, starter, pluginServer("weather", "forecast"))
	if err := mgr.Start(t.Context()); err == nil {
		t.Fatal("Start() succeeded without a sandbox")
	}
	status := mgr.PluginStatuses()[0]
	if status.Code != ErrServerUnavailable || status.Connected || len(status.Tools) != 0 {
		t.Fatalf("status = %+v", status)
	}
}

func TestManagerHonoursThePluginCapabilityGate(t *testing.T) {
	var prompts int
	broker := countingBroker(t, &prompts)
	starter := &stubSessionStarter{}
	mgr, err := NewManager(&ManagerOptions{
		Config:            &config.MCP{},
		Broker:            broker,
		SessionStarter:    starter,
		Plugins:           []PluginServer{pluginServer("weather", "forecast")},
		CapabilityChecker: func([]string) error { return errors.New("network is unavailable") },
	})
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close(context.WithoutCancel(t.Context())) })
	if err := mgr.Start(t.Context()); err == nil {
		t.Fatal("Start() ignored the capability gate")
	}
	if len(starter.requests) != 0 || mgr.PluginStatuses()[0].Code != ErrCapabilityUnavailable {
		t.Fatalf("requests = %d status = %+v", len(starter.requests), mgr.PluginStatuses()[0])
	}
}

func TestNewManagerRejectsUnsafePlugins(t *testing.T) {
	good := pluginServer("weather", "forecast")
	badID := pluginServer("Weather", "forecast")
	badBackend := pluginServer("weather", "forecast")
	badBackend.Server.Command = "relative/server"
	duplicateTool := pluginServer("weather", "forecast", "forecast")
	emptyTool := pluginServer("weather", "")
	sse := pluginServer("weather", "forecast")
	sse.Server.Transport = config.MCPTransportLegacySSE
	cases := map[string]struct {
		plugins []PluginServer
		servers []config.MCPServer
	}{
		"invalid id":       {plugins: []PluginServer{badID}},
		"duplicate id":     {plugins: []PluginServer{good, good}},
		"relative command": {plugins: []PluginServer{badBackend}},
		"duplicate tool":   {plugins: []PluginServer{duplicateTool}},
		"empty tool name":  {plugins: []PluginServer{emptyTool}},
		"legacy sse":       {plugins: []PluginServer{sse}},
		"name collision":   {plugins: []PluginServer{good}, servers: []config.MCPServer{startableStdioConfig("plugin-weather")}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var prompts int
			_, err := NewManager(&ManagerOptions{Config: &config.MCP{Servers: tc.servers}, Broker: countingBroker(t, &prompts), Plugins: tc.plugins})
			if code, ok := CodeOf(err); !ok || code != ErrConfigInvalid {
				t.Fatalf("NewManager error = %v, want %s", err, ErrConfigInvalid)
			}
		})
	}
}

func TestPluginToolsWithAnOverlongWireNameAreSkipped(t *testing.T) {
	var prompts int
	broker := countingBroker(t, &prompts)
	long := strings.Repeat("a", 60)
	starter := &stubSessionStarter{session: &scriptedSession{toolNames: []string{long}}}
	mgr := startPluginManager(t, broker, nil, starter, pluginServer("weather", long))
	if err := mgr.Start(t.Context()); err != nil {
		t.Fatalf("Start(): %v", err)
	}
	if names := pluginToolNames(broker); len(names) != 0 {
		t.Fatalf("plugin tools = %v", names)
	}
}

func TestPluginToolWithADisplayNeedsAStrictSchemaOrIsHiddenAsDrift(t *testing.T) {
	cases := []struct {
		name    string
		display []string
		schema  string
		exposed bool
	}{
		{"strict schema naming every field", []string{"to", "body"}, `{"type":"object","properties":{"to":{},"body":{},"cc":{}},"additionalProperties":false}`, true},
		{"no display needs no strictness", nil, `{"type":"object"}`, true},
		{"additional properties unset", []string{"to"}, `{"type":"object","properties":{"to":{}}}`, false},
		{"additional properties allowed", []string{"to"}, `{"type":"object","properties":{"to":{}},"additionalProperties":true}`, false},
		{"display names an undeclared field", []string{"to", "bcc"}, `{"type":"object","properties":{"to":{}},"additionalProperties":false}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var prompts int
			broker := countingBroker(t, &prompts)
			starter := &stubSessionStarter{session: &scriptedSession{toolNames: []string{"send"}, schemas: map[string]string{"send": tc.schema}}}
			server := pluginServer("mailer", "send")
			server.Tools[0].Display = tc.display
			mgr := startPluginManager(t, broker, nil, starter, server)
			if err := mgr.Start(t.Context()); err != nil {
				t.Fatalf("Start(): %v", err)
			}
			definition, registered := toolDefinition(broker, "plugin_mailer_send")
			if registered != tc.exposed {
				t.Fatalf("registered = %v, want %v", registered, tc.exposed)
			}
			status := mgr.PluginStatuses()[0]
			if tc.exposed {
				if !slices.Equal(definition.Display, tc.display) || len(status.Drift) != 0 {
					t.Fatalf("display = %v, drift = %v", definition.Display, status.Drift)
				}
				return
			}
			if !slices.Equal(status.Drift, []string{"send"}) || len(status.Tools) != 0 {
				t.Fatalf("status = %+v, want the tool listed as drift", status)
			}
		})
	}
}

func TestNewManagerRejectsInvalidPluginDisplayNames(t *testing.T) {
	for name, display := range map[string][]string{"repeated": {"to", "to"}, "path": {"a.b"}, "empty": {""}} {
		t.Run(name, func(t *testing.T) {
			server := pluginServer("mailer", "send")
			server.Tools[0].Display = display
			var prompts int
			_, err := NewManager(&ManagerOptions{Config: &config.MCP{}, Broker: countingBroker(t, &prompts), Plugins: []PluginServer{server}})
			if code, ok := CodeOf(err); !ok || code != ErrConfigInvalid {
				t.Fatalf("NewManager error = %v, want %s", err, ErrConfigInvalid)
			}
		})
	}
}
