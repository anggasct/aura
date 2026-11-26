package mcp

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"go.opentelemetry.io/otel/metric"

	"github.com/anggasct/aura/internal/config"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/capability"
	"github.com/anggasct/aura/internal/platform/egress"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/sandbox"
	"github.com/anggasct/aura/internal/platform/telemetry"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

const HealthCheckID = "mcp"

type ModuleDeps struct {
	Config         *config.MCP
	Plugins        []PluginServer
	Broker         *toolbroker.Broker
	TrustPath      string
	Resolver       egress.Resolver
	HTTPClient     *http.Client
	SessionStarter SessionStarter
	Meters         metric.MeterProvider
	Capabilities   capability.Report
	Personas       []auraagent.Definition
	CheckTimeout   time.Duration
	Logger         *slog.Logger
}

type Module struct {
	manager      *Manager
	servers      []config.MCPServer
	checkTimeout time.Duration
	logger       *slog.Logger
}

func New(ctx context.Context, deps *ModuleDeps) (*Module, error) {
	if deps == nil || deps.Broker == nil {
		return nil, Errorf(ErrConfigInvalid, "mcp module requires a tool broker")
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	var servers []config.MCPServer
	if deps.Config != nil {
		servers = deps.Config.Servers
	}
	if len(servers) == 0 && len(deps.Plugins) == 0 {
		return nil, Errorf(ErrConfigInvalid, "no mcp servers or plugins are configured")
	}
	mcpConfig := deps.Config
	if mcpConfig == nil {
		mcpConfig = &config.MCP{}
	}
	var registry TrustRegistry
	if len(servers) > 0 || deps.TrustPath != "" {
		fileRegistry, err := NewFileTrustRegistry(deps.TrustPath)
		if err != nil {
			return nil, err
		}
		registry = fileRegistry
	}
	httpClient := deps.HTTPClient
	if httpClient == nil {
		httpClient = NewHTTPClient(deps.Resolver)
	}
	starter := deps.SessionStarter
	if starter == nil {
		starter = NewSandboxSessionStarter(sandbox.Start)
	}
	var observer Observer
	if deps.Meters != nil {
		recorder, err := telemetry.NewMCPRecorder(deps.Meters)
		if err != nil {
			return nil, err
		}
		observer = NewTelemetryObserver(recorder)
	}
	manager, err := NewManager(&ManagerOptions{
		Config:            mcpConfig,
		Plugins:           deps.Plugins,
		Broker:            deps.Broker,
		TrustRegistry:     registry,
		HTTPClient:        httpClient,
		EndpointPolicy:    NewEndpointPolicy(deps.Resolver),
		SecretResolver:    NewSecretResolver(),
		SessionStarter:    starter,
		Observer:          observer,
		Logger:            logger,
		CapabilityChecker: deps.Capabilities.Require,
	})
	if err != nil {
		return nil, err
	}
	_ = manager.Start(ctx)
	running := 0
	statuses := manager.Statuses()
	for i := range statuses {
		if statuses[i].Code == "" {
			running++
		}
	}
	logger.InfoContext(ctx, "mcp servers started",
		"component", "mcp",
		"configured", len(statuses),
		"running", running,
		"degraded", len(statuses)-running,
	)
	mod := &Module{
		manager:      manager,
		servers:      slices.Clone(servers),
		checkTimeout: deps.CheckTimeout,
		logger:       logger,
	}
	mod.WarnUnexposedPersonaTools(ctx, deps.Personas)
	return mod, nil
}

func (m *Module) HealthChecks() []health.RegisteredCheck {
	if m == nil || len(m.servers) == 0 {
		return nil
	}
	return []health.RegisteredCheck{
		{
			ID:          HealthCheckID,
			Checker:     NewServerChecker(m.servers, m.manager.Statuses),
			Timeout:     m.checkTimeout,
			Remediation: RemediationReviewServer,
		},
	}
}

func (m *Module) Close(ctx context.Context) error {
	if m == nil || m.manager == nil {
		return nil
	}
	return m.manager.Close(ctx)
}

func (m *Module) Manager() *Manager {
	if m == nil {
		return nil
	}
	return m.manager
}

func (m *Module) PluginStatuses() []PluginStatus {
	if m == nil || m.manager == nil {
		return nil
	}
	return m.manager.PluginStatuses()
}

func (m *Module) WarnUnexposedPersonaTools(ctx context.Context, personas []auraagent.Definition) {
	if m == nil || m.manager == nil {
		return
	}
	for i := range personas {
		persona := &personas[i]
		for _, entry := range m.manager.UnexposedEntries(persona.Tools) {
			m.logger.WarnContext(ctx, "persona lists an mcp tool its server does not expose",
				"component", "mcp",
				"persona", persona.ID,
				"tool", entry,
			)
		}
	}
}

var _ health.HealthProvider = (*Module)(nil)
