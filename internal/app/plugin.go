package app

import (
	"context"
	"database/sql"
	"log/slog"
	"slices"
	"time"

	appconfig "github.com/anggasct/aura/internal/app/config"
	legacyconfig "github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/modules/mcp"
	"github.com/anggasct/aura/internal/modules/plugin"
	"github.com/anggasct/aura/internal/modules/plugin/auth"
	"github.com/anggasct/aura/internal/platform/capability"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

type discardRedactor struct{}

func (discardRedactor) RegisterSecret(string) {}

type pluginAuditAdapter struct {
	db *sql.DB
}

func (a *pluginAuditAdapter) Record(ctx context.Context, action, pluginID, version string) error {
	subject := pluginID
	if action == plugin.AuditActionApprove && version != "" {
		subject = pluginID + "-" + version
	}
	return recordAccessAction(ctx, a.db, &accessAction{
		Action:  action,
		Plugin:  pluginID,
		Version: version,
	}, subject, time.Now())
}

func openPluginService(cfg *appconfig.Config, db *sql.DB, redactor auth.Redactor) (*plugin.Service, error) {
	dataRoot, err := resolveDataRoot(cfg)
	if err != nil {
		return nil, err
	}
	return plugin.OpenService(db, dataRoot, redactor, nil)
}

func buildPluginModule(ctx context.Context, cfg *appconfig.Config, db *sql.DB, broker *toolbroker.Broker, capabilities capability.Report, logger *slog.Logger) (*plugin.Module, error) {
	dataRoot, err := resolveDataRoot(cfg)
	if err != nil {
		return nil, err
	}
	return plugin.New(ctx, &plugin.ModuleDeps{
		Config:       cfg.Plugins,
		DB:           db,
		DataRoot:     dataRoot,
		Broker:       broker,
		Capabilities: capabilities,
		Validator:    mcp.MakeValidator,
		Logger:       logger,
	})
}

func mcpServerFromBackend(s *plugin.BackendServer) legacyconfig.MCPServer {
	return legacyconfig.MCPServer{
		Name:           s.Name,
		Transport:      s.Transport,
		Command:        s.Command,
		Args:           slices.Clone(s.Args),
		URL:            s.URL,
		StartupTimeout: s.StartupTimeout,
		RequestTimeout: s.RequestTimeout,
		ConnectTimeout: s.ConnectTimeout,
		MaxMessageSize: s.MaxMessageSize,
		Capabilities:   slices.Clone(s.Capabilities),
	}
}

func mcpPluginsFromBackends(backends []plugin.Active) []mcp.PluginServer {
	servers := make([]mcp.PluginServer, 0, len(backends))
	for i := range backends {
		active := &backends[i]
		if active.Kind != plugin.BackendMCP {
			continue
		}
		server := mcp.PluginServer{
			ID:         active.ID,
			Server:     mcpServerFromBackend(&active.Server),
			Authorizer: active.Auth,
		}
		for _, tool := range active.Tools {
			server.Tools = append(server.Tools, mcp.PluginTool{
				Name:        tool.Name,
				Description: tool.Description,
				Display:     slices.Clone(tool.Display),
			})
		}
		servers = append(servers, server)
	}
	return servers
}

func mcpPluginStatuses(mcpServers *mcpRuntime) []plugin.RuntimeStatus {
	if mcpServers == nil || mcpServers.manager == nil {
		return nil
	}
	statuses := mcpServers.manager.PluginStatuses()
	out := make([]plugin.RuntimeStatus, 0, len(statuses))
	for _, status := range statuses {
		out = append(out, plugin.RuntimeStatus{
			ID:      status.ID,
			Running: status.Code == "" && status.Connected,
			Drift:   status.Drift,
		})
	}
	return out
}

func pluginHealthCheck(mod *plugin.Module, mcpServers *mcpRuntime, timeout time.Duration) (health.RegisteredCheck, bool, error) {
	if mod == nil {
		return health.RegisteredCheck{}, false, nil
	}
	var mcpStatus func() []plugin.RuntimeStatus
	if mcpServers != nil {
		mcpStatus = func() []plugin.RuntimeStatus {
			return mcpPluginStatuses(mcpServers)
		}
	}
	return mod.HealthCheck(mcpStatus, timeout)
}
