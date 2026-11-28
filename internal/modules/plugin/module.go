package plugin

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/modules/plugin/auth"
	"github.com/anggasct/aura/internal/modules/plugin/connector"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/capability"
	"github.com/anggasct/aura/internal/platform/egress"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/sandbox"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/workspace"
)

const (
	pluginsDirName = "plugins"
	secretsDirName = "plugin-secrets"
)

type discardRedactor struct{}

func (discardRedactor) RegisterSecret(string) {}

type ModuleDeps struct {
	Config              *Config
	DB                  *sql.DB
	DataRoot            string
	Broker              *toolbroker.Broker
	Redactor            auth.Redactor
	Capabilities        capability.Report
	Validator           connector.SchemaValidator
	AuthHTTPClient      *http.Client
	ConnectorHTTPClient *http.Client
	Starter             connector.Starter
	TaintTracker        *workspace.Tracker
	Logger              *slog.Logger
}

type Module struct {
	cfg        *Config
	registry   *SQLiteStore
	service    *Service
	plan       *Plan
	installed  int
	connectors *connector.Manager
	logger     *slog.Logger
}

func OpenService(db *sql.DB, dataRoot string, redactor auth.Redactor, authClient *http.Client) (*Service, error) {
	service, _, err := openServiceWithStore(db, dataRoot, redactor, authClient)
	return service, err
}

func openServiceWithStore(db *sql.DB, dataRoot string, redactor auth.Redactor, authClient *http.Client) (*Service, *SQLiteStore, error) {
	if db == nil {
		return nil, nil, fault.New(ErrInvalidArgument, "database must not be nil")
	}
	if strings.TrimSpace(dataRoot) == "" {
		return nil, nil, fault.New(ErrInvalidArgument, "data root must not be empty")
	}
	absolute, err := filepath.Abs(dataRoot)
	if err != nil {
		return nil, nil, fault.New(ErrStorage, "the data directory cannot be resolved")
	}
	store, err := NewSQLiteStore(db)
	if err != nil {
		return nil, nil, err
	}
	if redactor == nil {
		redactor = discardRedactor{}
	}
	if authClient == nil {
		authClient = auth.NewHTTPClient(nil)
	}
	authRegistry, err := auth.NewRegistry(&auth.Options{
		Dir:        filepath.Join(absolute, secretsDirName),
		Secrets:    auth.NewSecretResolver(),
		HTTPClient: authClient,
		Redactor:   redactor,
	})
	if err != nil {
		return nil, nil, err
	}
	service, err := NewService(&Options{
		Registry: store,
		Auth:     authRegistry,
		Dir:      filepath.Join(absolute, pluginsDirName),
	})
	if err != nil {
		return nil, nil, err
	}
	return service, store, nil
}

func New(ctx context.Context, deps *ModuleDeps) (*Module, error) {
	if ctx == nil || deps == nil {
		return nil, fault.New(ErrInvalidArgument, "context and module dependencies are required")
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	redactor := deps.Redactor
	if redactor == nil && deps.Broker != nil {
		redactor = deps.Broker
	}
	authClient := deps.AuthHTTPClient
	if authClient == nil {
		authClient = auth.NewHTTPClient(nil)
	}
	connectorClient := deps.ConnectorHTTPClient
	if connectorClient == nil {
		connectorClient = egress.NewAPIClient(nil)
	}
	service, store, err := openServiceWithStore(deps.DB, deps.DataRoot, redactor, authClient)
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
	mod := &Module{
		cfg:       deps.Config,
		registry:  store,
		service:   service,
		plan:      plan,
		installed: len(summaries),
		logger:    logger,
	}
	connectorConfigs := connectorConfigsFromPlan(plan)
	if len(connectorConfigs) > 0 && deps.Broker != nil {
		starter := deps.Starter
		if starter == nil {
			starter = connector.NewSandboxStarter(sandbox.Start)
		}
		manager, err := connector.NewManager(&connector.ManagerOptions{
			Plugins:           connectorConfigs,
			Broker:            deps.Broker,
			Starter:           starter,
			HTTPClient:        connectorClient,
			Validator:         deps.Validator,
			CapabilityChecker: deps.Capabilities.Require,
			TaintTracker:      deps.TaintTracker,
			Logger:            logger,
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
		logger.InfoContext(ctx, "connector plugins started",
			"component", "plugin_connector",
			"configured", len(statuses),
			"running", running,
			"degraded", len(statuses)-running,
		)
		mod.connectors = manager
	}
	return mod, nil
}

func connectorConfigsFromPlan(plan *Plan) []connector.Config {
	if plan == nil {
		return nil
	}
	configs := make([]connector.Config, 0, len(plan.Active))
	for i := range plan.Active {
		active := &plan.Active[i]
		if active.Kind != BackendConnector {
			continue
		}
		cfg := connector.Config{
			ID:              active.ID,
			Executable:      active.Server.Command,
			Arguments:       append([]string(nil), active.Server.Args...),
			WorkingDir:      filepath.Clean(active.Root),
			StartupTimeout:  time.Duration(active.Server.StartupTimeout),
			RequestTimeout:  time.Duration(active.Server.RequestTimeout),
			MaxMessageBytes: int64(active.Server.MaxMessageSize),
			Capabilities:    append([]string(nil), active.Server.Capabilities...),
			Egress:          append([]string(nil), active.Egress...),
			Auth:            active.Auth,
			AuthHosts:       slices.Clone(active.AuthHosts),
		}
		for _, tool := range active.Tools {
			cfg.Tools = append(cfg.Tools, connector.Tool{
				Name:        tool.Name,
				Description: tool.Description,
				Effect:      tool.Tier == TierEffect,
				Display:     slices.Clone(tool.Display),
			})
		}
		configs = append(configs, cfg)
	}
	return configs
}

func (m *Module) Service() *Service {
	if m == nil {
		return nil
	}
	return m.service
}

func (m *Module) Plan() *Plan {
	if m == nil {
		return nil
	}
	return m.plan
}

func (m *Module) MCPBackends() []Active {
	if m == nil || m.plan == nil {
		return nil
	}
	var out []Active
	for i := range m.plan.Active {
		if m.plan.Active[i].Kind == BackendMCP {
			out = append(out, m.plan.Active[i])
		}
	}
	return out
}

func (m *Module) ConnectorStatuses() []connector.Status {
	if m == nil || m.connectors == nil {
		return nil
	}
	return m.connectors.Statuses()
}

func (m *Module) HasConnectors() bool {
	return m != nil && m.connectors != nil
}

func (m *Module) AuthStatus(ctx context.Context, id string) ErrorCode {
	if m == nil || m.plan == nil {
		return ""
	}
	for i := range m.plan.Active {
		if m.plan.Active[i].ID == id {
			return m.plan.Active[i].AuthStatus(ctx)
		}
	}
	return ""
}

func (m *Module) HealthCheck(mcpStatus func() []RuntimeStatus, checkTimeout time.Duration) (health.RegisteredCheck, bool, error) {
	if m == nil || (m.cfg == nil && m.installed == 0) {
		return health.RegisteredCheck{}, false, nil
	}
	var source func(context.Context) []RuntimeStatus
	if mcpStatus != nil || m.connectors != nil {
		source = func(ctx context.Context) []RuntimeStatus {
			var out []RuntimeStatus
			if mcpStatus != nil {
				for _, status := range mcpStatus() {
					if status.Auth == "" {
						status.Auth = m.AuthStatus(ctx, status.ID)
					}
					out = append(out, status)
				}
			}
			if m.connectors != nil {
				for _, status := range m.connectors.Statuses() {
					out = append(out, RuntimeStatus{
						ID:      status.ID,
						Running: status.Code == "",
						Drift:   status.Drift,
						Auth:    m.AuthStatus(ctx, status.ID),
						Backend: ErrorCode(status.Code),
					})
				}
			}
			return out
		}
	}
	checker, err := NewChecker(&CheckerOptions{
		Registry: m.registry,
		Desired:  m.cfg,
		Held:     m.plan.Held,
		Runtime:  source,
	})
	if err != nil {
		return health.RegisteredCheck{}, false, err
	}
	return health.RegisteredCheck{
		ID:          HealthCheckID,
		Checker:     checker,
		Timeout:     checkTimeout,
		Remediation: RemediationReview,
	}, true, nil
}

func (m *Module) WarnUnavailable(ctx context.Context, personas []auraagent.Definition) {
	if m == nil || m.plan == nil {
		return
	}
	running := make([]string, 0, len(m.plan.Active))
	for i := range m.plan.Active {
		running = append(running, m.plan.Active[i].ID)
	}
	for i := range personas {
		for _, id := range personas[i].Plugins {
			if !slices.Contains(running, id) {
				m.logger.WarnContext(ctx, "persona lists a plugin that is not running", "component", "plugin", "persona", personas[i].ID, "plugin", id)
			}
		}
	}
}

func (m *Module) Close(ctx context.Context) error {
	if m == nil || m.connectors == nil {
		return nil
	}
	return m.connectors.Close(ctx)
}
