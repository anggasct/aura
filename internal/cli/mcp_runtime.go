package cli

import (
	"context"
	"errors"
	"log/slog"
	"time"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/capability"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/egress"
	"github.com/anggasct/aura/internal/health"
	"github.com/anggasct/aura/internal/mcp"
	"github.com/anggasct/aura/internal/sandbox"
	"github.com/anggasct/aura/internal/telemetry"
	"github.com/anggasct/aura/internal/toolbroker"
	"go.opentelemetry.io/otel/metric"
)

const mcpHealthCheckID = "mcp"

func mcpServersConfigured(cfg *config.Config) bool {
	return len(configuredMCPServers(cfg)) > 0
}

type mcpRuntimeOptions struct {
	Config         *config.Config
	Broker         *toolbroker.Broker
	Logger         *slog.Logger
	Recorder       *telemetry.MCPRecorder
	Capabilities   capability.Report
	SessionStarter mcp.SessionStarter
	Resolver       egress.Resolver
	TrustPath      string
	Plugins        []mcp.PluginServer
}

type mcpRuntime struct {
	manager  *mcp.Manager
	check    health.RegisteredCheck
	hasCheck bool
}

func (r *mcpRuntime) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	return r.manager.Close(ctx)
}

func startMCPRuntime(ctx context.Context, opts *mcpRuntimeOptions) (*mcpRuntime, error) {
	if opts == nil || opts.Config == nil || opts.Broker == nil || opts.Logger == nil {
		return nil, errors.New("mcp runtime requires configuration, a tool broker and a logger")
	}
	servers := configuredMCPServers(opts.Config)
	if len(servers) == 0 && len(opts.Plugins) == 0 {
		return nil, errors.New("no mcp servers or plugins are configured")
	}
	mcpConfig := opts.Config.MCP
	if mcpConfig == nil {
		mcpConfig = &config.MCP{}
	}
	registry, err := mcp.NewFileTrustRegistry(opts.TrustPath)
	if err != nil {
		return nil, err
	}
	starter := opts.SessionStarter
	if starter == nil {
		starter = newMCPSessionStarter(sandbox.Start)
	}
	manager, err := mcp.NewManager(&mcp.ManagerOptions{
		Config:            mcpConfig,
		Plugins:           opts.Plugins,
		Broker:            opts.Broker,
		TrustRegistry:     registry,
		HTTPClient:        mcpHTTPClient(opts.Resolver),
		EndpointPolicy:    mcpEndpointPolicy{resolver: opts.Resolver},
		SecretResolver:    mcpSecretResolver{},
		SessionStarter:    starter,
		Observer:          mcpRecorderObserver(opts.Recorder),
		Logger:            opts.Logger,
		CapabilityChecker: opts.Capabilities.Require,
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
	opts.Logger.InfoContext(ctx, "mcp servers started",
		"component", "mcp",
		"configured", len(statuses),
		"running", running,
		"degraded", len(statuses)-running,
	)
	runtime := &mcpRuntime{manager: manager}
	if len(servers) > 0 {
		runtime.check = health.RegisteredCheck{
			ID:          mcpHealthCheckID,
			Checker:     mcp.NewServerChecker(servers, manager.Statuses),
			Timeout:     time.Duration(opts.Config.Health.CheckTimeout),
			Remediation: mcp.RemediationReviewServer,
		}
		runtime.hasCheck = true
	}
	return runtime, nil
}

func (r *mcpRuntime) warnUnexposedPersonaTools(ctx context.Context, logger *slog.Logger, personas []auraagent.Definition) {
	if r == nil {
		return
	}
	for i := range personas {
		persona := &personas[i]
		for _, entry := range r.manager.UnexposedEntries(persona.Tools) {
			logger.WarnContext(ctx, "persona lists an mcp tool its server does not expose",
				"component", "mcp",
				"persona", persona.ID,
				"tool", entry,
			)
		}
	}
}

func startConfiguredMCP(ctx context.Context, cfg *config.Config, broker *toolbroker.Broker, logger *slog.Logger, meters metric.MeterProvider, capabilities capability.Report, personas []auraagent.Definition, plugins []mcp.PluginServer) (*mcpRuntime, error) {
	recorder, err := telemetry.NewMCPRecorder(meters)
	if err != nil {
		return nil, err
	}
	trustPath, err := mcpTrustRegistryPath(cfg)
	if err != nil {
		return nil, err
	}
	started, err := startMCPRuntime(ctx, &mcpRuntimeOptions{
		Config:       cfg,
		Broker:       broker,
		Logger:       logger,
		Recorder:     recorder,
		Capabilities: capabilities,
		TrustPath:    trustPath,
		Plugins:      plugins,
	})
	if err != nil {
		return nil, err
	}
	started.warnUnexposedPersonaTools(ctx, logger, personas)
	return started, nil
}
