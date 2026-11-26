package app

import (
	"context"
	"database/sql"
	"log/slog"
	"slices"
	"time"

	appconfig "github.com/anggasct/aura/internal/app/config"
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

func mcpServerFromBackend(s *plugin.BackendServer) mcp.ServerConfig {
	out := mcp.ServerConfig{
		Name:           s.Name,
		Transport:      s.Transport,
		Command:        s.Command,
		Args:           slices.Clone(s.Args),
		URL:            s.URL,
		StartupTimeout: s.StartupTimeout,
		RequestTimeout: s.RequestTimeout,
		MaxMessageSize: s.MaxMessageSize,
		Capabilities:   slices.Clone(s.Capabilities),
	}
	if s.ConnectTimeout > 0 {
		ct := s.ConnectTimeout
		out.ConnectTimeout = &ct
	}
	return out
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

func mcpPluginStatuses(mcpServers *mcp.Module) []plugin.RuntimeStatus {
	if mcpServers == nil {
		return nil
	}
	statuses := mcpServers.PluginStatuses()
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

func pluginHealthCheck(mod *plugin.Module, mcpServers *mcp.Module, timeout time.Duration) (health.RegisteredCheck, bool, error) {
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
