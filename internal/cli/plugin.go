package cli

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"time"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/health"
	"github.com/anggasct/aura/internal/mcp"
	"github.com/anggasct/aura/internal/plugin"
	"github.com/anggasct/aura/internal/plugin/auth"
	"github.com/anggasct/aura/internal/store"
)

const (
	pluginDirName        = "plugins"
	pluginSecretsDirName = "plugin-secrets"

	accessActionPluginApprove = "plugin_approve"
	accessActionPluginDisable = "plugin_disable"
	accessActionPluginRemove  = "plugin_remove"
	accessActionPluginLogin   = "plugin_login"
	accessActionPluginLogout  = "plugin_logout"
)

type discardRedactor struct{}

func (discardRedactor) RegisterSecret(string) {}

func pluginDirectory(cfg *config.Config, name string) (string, error) {
	dataRoot, err := resolveDataRoot(cfg)
	if err != nil {
		return "", err
	}
	absolute, err := filepath.Abs(dataRoot)
	if err != nil {
		return "", plugin.Wrap(plugin.ErrStorage, err, "the data directory cannot be resolved")
	}
	return filepath.Join(absolute, name), nil
}

func newPluginAuth(cfg *config.Config, redactor auth.Redactor) (*auth.Registry, error) {
	dir, err := pluginDirectory(cfg, pluginSecretsDirName)
	if err != nil {
		return nil, err
	}
	return auth.NewRegistry(&auth.Options{Dir: dir, Secrets: mcpSecretResolver{}, HTTPClient: pluginHTTPClient(mcpHTTPClient(nil)), Redactor: redactor})
}

type loopbackAwareTransport struct {
	loopback http.RoundTripper
	external http.RoundTripper
}

func (t *loopbackAwareTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req != nil && req.URL != nil && config.IsLoopbackBaseURL(req.URL.String()) {
		return t.loopback.RoundTrip(req)
	}
	return t.external.RoundTrip(req)
}

func (t *loopbackAwareTransport) CloseIdleConnections() {
	for _, next := range []http.RoundTripper{t.loopback, t.external} {
		if closer, ok := next.(interface{ CloseIdleConnections() }); ok {
			closer.CloseIdleConnections()
		}
	}
}

func pluginHTTPClient(external *http.Client) *http.Client {
	return &http.Client{Transport: &loopbackAwareTransport{loopback: http.DefaultTransport, external: external.Transport}}
}

func newPluginService(cfg *config.Config, db *sql.DB, redactor auth.Redactor) (*plugin.Service, error) {
	dir, err := pluginDirectory(cfg, pluginDirName)
	if err != nil {
		return nil, err
	}
	authRegistry, err := newPluginAuth(cfg, redactor)
	if err != nil {
		return nil, err
	}
	return plugin.NewService(&plugin.Options{Registry: &storePluginRegistry{plugins: store.NewPluginStore(db)}, Auth: authRegistry, Dir: dir})
}

type storePluginRegistry struct {
	plugins store.PluginStore
}

func pluginStoreError(err error, conflict plugin.ErrorCode) error {
	if err == nil {
		return nil
	}
	code, ok := store.CodeOf(err)
	if !ok {
		return plugin.Wrap(plugin.ErrStorage, err, "the plugin registry is unavailable")
	}
	switch code {
	case store.ErrorCodePluginNotFound:
		return plugin.Wrap(plugin.ErrNotFound, err, "the plugin or version is not installed")
	case store.ErrorCodePluginConflict:
		return plugin.Wrap(conflict, err, "the plugin registry rejected the change")
	case store.ErrorCodePluginInvalid:
		return plugin.Wrap(plugin.ErrInvalidArgument, err, "the plugin registry rejected the record")
	default:
		return plugin.Wrap(plugin.ErrStorage, err, "the plugin registry is unavailable")
	}
}

func (r *storePluginRegistry) Install(ctx context.Context, in *plugin.InstallRecord) error {
	record := store.PluginInstall{
		Plugin:  toStorePlugin(&in.Plugin),
		Version: store.PluginVersion{PluginID: in.Version.ID, Version: in.Version.Version, Digest: in.Version.Digest, ManifestJSON: in.Version.ManifestJSON, InstalledAt: in.Version.InstalledAt},
	}
	for _, key := range in.DropGrants {
		record.DropGrants = append(record.DropGrants, store.PluginGrantKey{Surface: key.Surface, Value: key.Value})
	}
	return pluginStoreError(r.plugins.Install(ctx, &record), plugin.ErrVersionConflict)
}

func (r *storePluginRegistry) Approve(ctx context.Context, in *plugin.ApprovalRecord) error {
	record := store.PluginApproval{PluginID: in.ID, Version: in.Version, Digest: in.Digest, At: in.At}
	for i := range in.Grants {
		grant := &in.Grants[i]
		record.Grants = append(record.Grants, store.PluginGrant{
			PluginID: grant.ID, Surface: grant.Surface, Value: grant.Value, Fingerprint: grant.Fingerprint, ApprovedAt: grant.ApprovedAt, ApprovedBy: grant.ApprovedBy,
		})
	}
	return pluginStoreError(r.plugins.Approve(ctx, &record), plugin.ErrDigestMismatch)
}

func (r *storePluginRegistry) SetState(ctx context.Context, id, state string, at time.Time) error {
	return pluginStoreError(r.plugins.SetState(ctx, id, state, at), plugin.ErrInvalidArgument)
}

func (r *storePluginRegistry) Get(ctx context.Context, id string) (plugin.Record, error) {
	row, err := r.plugins.Get(ctx, id)
	if err != nil {
		return plugin.Record{}, pluginStoreError(err, plugin.ErrInvalidArgument)
	}
	return fromStorePlugin(&row), nil
}

func (r *storePluginRegistry) List(ctx context.Context) ([]plugin.Record, error) {
	rows, err := r.plugins.List(ctx)
	if err != nil {
		return nil, pluginStoreError(err, plugin.ErrInvalidArgument)
	}
	records := make([]plugin.Record, 0, len(rows))
	for i := range rows {
		records = append(records, fromStorePlugin(&rows[i]))
	}
	return records, nil
}

func (r *storePluginRegistry) Version(ctx context.Context, id, version string) (plugin.VersionRecord, error) {
	row, err := r.plugins.Version(ctx, id, version)
	if err != nil {
		return plugin.VersionRecord{}, pluginStoreError(err, plugin.ErrInvalidArgument)
	}
	return plugin.VersionRecord{ID: row.PluginID, Version: row.Version, Digest: row.Digest, ManifestJSON: row.ManifestJSON, InstalledAt: row.InstalledAt}, nil
}

func (r *storePluginRegistry) Grants(ctx context.Context, id string) ([]plugin.GrantRecord, error) {
	rows, err := r.plugins.Grants(ctx, id)
	if err != nil {
		return nil, pluginStoreError(err, plugin.ErrInvalidArgument)
	}
	grants := make([]plugin.GrantRecord, 0, len(rows))
	for i := range rows {
		grants = append(grants, plugin.GrantRecord{
			ID: rows[i].PluginID, GrantKey: plugin.GrantKey{Surface: rows[i].Surface, Value: rows[i].Value},
			Fingerprint: rows[i].Fingerprint, ApprovedAt: rows[i].ApprovedAt, ApprovedBy: rows[i].ApprovedBy,
		})
	}
	return grants, nil
}

func (r *storePluginRegistry) Remove(ctx context.Context, id string) error {
	return pluginStoreError(r.plugins.Remove(ctx, id), plugin.ErrInvalidArgument)
}

func toStorePlugin(record *plugin.Record) store.Plugin {
	return store.Plugin{ID: record.ID, State: record.State, Version: record.Version, ApprovedVersion: record.ApprovedVersion, UpdatedAt: record.UpdatedAt}
}

func fromStorePlugin(row *store.Plugin) plugin.Record {
	return plugin.Record{ID: row.ID, State: row.State, Version: row.Version, ApprovedVersion: row.ApprovedVersion, UpdatedAt: row.UpdatedAt}
}

type pluginRuntime struct {
	service   *plugin.Service
	plan      *plugin.Plan
	installed int
}

func planPlugins(ctx context.Context, cfg *config.Config, db *sql.DB, redactor auth.Redactor) (*pluginRuntime, error) {
	service, err := newPluginService(cfg, db, redactor)
	if err != nil {
		return nil, err
	}
	summaries, err := service.List(ctx)
	if err != nil {
		return nil, err
	}
	plan, err := service.Plan(ctx)
	if err != nil {
		return nil, err
	}
	return &pluginRuntime{service: service, plan: plan, installed: len(summaries)}, nil
}

func (r *pluginRuntime) mcpPlugins() []mcp.PluginServer {
	if r == nil {
		return nil
	}
	servers := make([]mcp.PluginServer, 0, len(r.plan.Active))
	for i := range r.plan.Active {
		active := &r.plan.Active[i]
		server := mcp.PluginServer{ID: active.ID, Server: active.Server, Authorizer: active.Auth}
		for _, tool := range active.Tools {
			server.Tools = append(server.Tools, mcp.PluginTool{Name: tool.Name, Description: tool.Description})
		}
		servers = append(servers, server)
	}
	return servers
}

func (r *pluginRuntime) healthCheck(cfg *config.Config, db *sql.DB, mcpServers *mcpRuntime) (health.RegisteredCheck, bool, error) {
	if r == nil || (cfg.Plugins == nil && r.installed == 0) {
		return health.RegisteredCheck{}, false, nil
	}
	var source func(context.Context) []plugin.RuntimeStatus
	if mcpServers != nil {
		source = func(ctx context.Context) []plugin.RuntimeStatus {
			statuses := mcpServers.manager.PluginStatuses()
			out := make([]plugin.RuntimeStatus, 0, len(statuses))
			for i := range statuses {
				out = append(out, plugin.RuntimeStatus{
					ID: statuses[i].ID, Running: statuses[i].Code == "" && statuses[i].Connected, Drift: statuses[i].Drift,
					Auth: r.authStatus(ctx, statuses[i].ID),
				})
			}
			return out
		}
	}
	checker, err := plugin.NewChecker(&plugin.CheckerOptions{
		Registry: &storePluginRegistry{plugins: store.NewPluginStore(db)},
		Desired:  cfg.Plugins,
		Held:     r.plan.Held,
		Runtime:  source,
	})
	if err != nil {
		return health.RegisteredCheck{}, false, err
	}
	return health.RegisteredCheck{
		ID:          plugin.HealthCheckID,
		Checker:     checker,
		Timeout:     time.Duration(cfg.Health.CheckTimeout),
		Remediation: plugin.RemediationReview,
	}, true, nil
}

func (r *pluginRuntime) authStatus(ctx context.Context, id string) plugin.ErrorCode {
	for i := range r.plan.Active {
		if r.plan.Active[i].ID == id {
			return r.plan.Active[i].AuthStatus(ctx)
		}
	}
	return ""
}

func (r *pluginRuntime) warnUnavailablePlugins(ctx context.Context, logger *slog.Logger, personas []auraagent.Definition) {
	if r == nil {
		return
	}
	running := make([]string, 0, len(r.plan.Active))
	for i := range r.plan.Active {
		running = append(running, r.plan.Active[i].ID)
	}
	for i := range personas {
		for _, id := range personas[i].Plugins {
			if !slices.Contains(running, id) {
				logger.WarnContext(ctx, "persona lists a plugin that is not running", "component", "plugin", "persona", personas[i].ID, "plugin", id)
			}
		}
	}
}
