package cli

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"time"

	"github.com/anggasct/aura/internal/capability"
	"github.com/anggasct/aura/internal/egress"
	"github.com/anggasct/aura/internal/mcp"
	"github.com/anggasct/aura/internal/plugin"
	"github.com/anggasct/aura/internal/plugin/connector"
	"github.com/anggasct/aura/internal/sandbox"
	"github.com/anggasct/aura/internal/toolbroker"
)

type connectorRuntimeOptions struct {
	Broker       *toolbroker.Broker
	Logger       *slog.Logger
	Capabilities capability.Report
	Starter      mcp.SessionStarter
	HTTPClient   *http.Client
	Plugins      []connector.Config
}

func (r *pluginRuntime) connectorPlugins() []connector.Config {
	if r == nil {
		return nil
	}
	configs := make([]connector.Config, 0, len(r.plan.Active))
	for i := range r.plan.Active {
		active := &r.plan.Active[i]
		if active.Kind != plugin.BackendConnector {
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
			cfg.Tools = append(cfg.Tools, connector.Tool{Name: tool.Name, Description: tool.Description, Effect: tool.Tier == plugin.TierEffect, Display: slices.Clone(tool.Display)})
		}
		configs = append(configs, cfg)
	}
	return configs
}

func startConnectorPlugins(ctx context.Context, opts *connectorRuntimeOptions) (*connector.Manager, error) {
	if opts == nil || opts.Broker == nil || opts.Logger == nil {
		return nil, errors.New("connector plugins require a tool broker and a logger")
	}
	if len(opts.Plugins) == 0 {
		return nil, errors.New("no connector plugins are configured")
	}
	starter := opts.Starter
	if starter == nil {
		starter = newConnectorSessionStarter(sandbox.Start)
	}
	client := opts.HTTPClient
	if client == nil {
		client = egress.NewAPIClient(nil)
	}
	manager, err := connector.NewManager(&connector.ManagerOptions{
		Plugins:           opts.Plugins,
		Broker:            opts.Broker,
		Starter:           connectorStarter{inner: starter},
		HTTPClient:        client,
		Validator:         mcp.MakeValidator,
		CapabilityChecker: opts.Capabilities.Require,
		Logger:            opts.Logger,
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
	opts.Logger.InfoContext(ctx, "connector plugins started",
		"component", "plugin_connector",
		"configured", len(statuses),
		"running", running,
		"degraded", len(statuses)-running,
	)
	return manager, nil
}

const (
	connectorMemoryBytes  = 512 << 20
	connectorMaxProcesses = 256
	connectorMaxOpenFiles = 256
	connectorFileBytes    = 64 << 20
)

type sandboxSessionStart func(ctx context.Context, req *sandbox.SessionRequest) (*sandbox.Session, error)

func newConnectorSessionStarter(start sandboxSessionStart) mcp.SessionStarter {
	return newMCPSessionStarter(func(ctx context.Context, req *sandbox.SessionRequest) (*sandbox.Session, error) {
		req.Profile = sandbox.ProfileConnector
		req.Limits.MemoryBytes = connectorMemoryBytes
		req.Limits.MaxProcesses = connectorMaxProcesses
		req.Limits.MaxOpenFiles = connectorMaxOpenFiles
		req.Limits.FileBytes = connectorFileBytes
		return start(ctx, req)
	})
}

type connectorStarter struct {
	inner mcp.SessionStarter
}

func (s connectorStarter) StartSession(ctx context.Context, req *connector.SessionRequest) (connector.Session, error) {
	session, err := s.inner.StartSession(ctx, &mcp.ContainedSessionRequest{
		RequestID:      req.RequestID,
		ServerName:     "plugin-" + req.Name,
		Executable:     req.Executable,
		Arguments:      append([]string(nil), req.Arguments...),
		WorkingDir:     req.WorkingDir,
		Environment:    map[string]string{},
		Timeout:        req.Timeout,
		SetupTimeout:   req.SetupTimeout,
		MaxOutputBytes: req.MaxOutputBytes,
	})
	if err != nil {
		return nil, err
	}
	return connectorSession{inner: session}, nil
}

type connectorSession struct {
	inner mcp.ContainedSession
}

func (s connectorSession) Request(ctx context.Context, payload []byte) ([]byte, error) {
	out, err := s.inner.Request(ctx, payload)
	if err != nil {
		if code, coded := mcp.CodeOf(err); coded && code == mcp.ErrMessageTooLarge {
			return nil, connector.Errorf(connector.ErrRPCLimit, "the connector message exceeds the size bound")
		}
		return nil, err
	}
	return out, nil
}

func (s connectorSession) Close(ctx context.Context) error {
	return s.inner.Close(ctx)
}
