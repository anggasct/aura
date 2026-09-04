package mcp

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/toolbroker"
	"github.com/anggasct/aura/internal/tools"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type ManagerOptions struct {
	Config            *config.MCP
	Broker            *toolbroker.Broker
	TrustRegistry     TrustRegistry
	Logger            *slog.Logger
	CapabilityChecker func([]string) error
}

type Manager struct {
	cfg               *config.MCP
	broker            *toolbroker.Broker
	trustRegistry     TrustRegistry
	logger            *slog.Logger
	capabilityChecker func([]string) error
	clients           map[string]*Client
	registeredTools   map[string][]string
	customTransports  map[string]sdk.Transport
	mu                sync.Mutex
	closed            bool
}

func NewManager(opts ManagerOptions) (*Manager, error) {
	if opts.Config == nil {
		return nil, Errorf(ErrConfigInvalid, "mcp configuration is required")
	}
	if opts.Broker == nil {
		return nil, Errorf(ErrConfigInvalid, "tool broker is required")
	}
	if opts.TrustRegistry == nil {
		opts.TrustRegistry = NewMemoryTrustRegistry()
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	return &Manager{
		cfg:               opts.Config,
		broker:            opts.Broker,
		trustRegistry:     opts.TrustRegistry,
		logger:            opts.Logger,
		capabilityChecker: opts.CapabilityChecker,
		clients:           make(map[string]*Client),
		registeredTools:   make(map[string][]string),
		customTransports:  make(map[string]sdk.Transport),
	}, nil
}

func (m *Manager) SetCustomTransport(serverName string, transport sdk.Transport) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.customTransports[serverName] = transport
}

func (m *Manager) Start(ctx context.Context) error {
	if ctx == nil {
		return Errorf(ErrConfigInvalid, "context must not be nil")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Errorf(ErrServerUnavailable, "manager is closed")
	}

	for i := range m.cfg.Servers {
		serverCfg := &m.cfg.Servers[i]

		if m.capabilityChecker != nil && len(serverCfg.Capabilities) > 0 {
			if err := m.capabilityChecker(serverCfg.Capabilities); err != nil {
				return Wrap(ErrCapabilityUnavailable, err, "required capabilities unavailable")
			}
		}

		client, err := NewClient(serverCfg, m.logger)
		if err != nil {
			return err
		}

		transport := m.customTransports[serverCfg.Name]
		if err := client.Connect(ctx, transport); err != nil {
			return err
		}

		discovered, err := client.DiscoverTools(ctx)
		if err != nil {
			_ = client.Close()
			return err
		}

		digest, err := ComputeTrustDigest(serverCfg, discovered)
		if err != nil {
			_ = client.Close()
			return err
		}

		trusted, err := m.trustRegistry.IsTrusted(ctx, serverCfg.Name, digest)
		if err != nil {
			_ = client.Close()
			return Wrap(ErrResultInvalid, err, "failed to check trust")
		}

		if !trusted {
			toolNames := make([]string, 0, len(discovered))
			for _, t := range discovered {
				toolNames = append(toolNames, t.Name)
			}
			_ = m.trustRegistry.SaveTrust(ctx, &TrustRecord{
				ServerName:   serverCfg.Name,
				Digest:       digest,
				Decision:     TrustDecisionPending,
				Capabilities: slices.Clone(serverCfg.Capabilities),
				Tools:        toolNames,
			})
			_ = client.Close()
			return Errorf(ErrTrustRequired, "server %q trust review required for digest %s", serverCfg.Name, digest)
		}

		registeredNames := make([]string, 0, len(discovered))
		for _, tool := range discovered {
			namespaced := FormatToolName(serverCfg.Name, tool.Name)
			version := "v1"

			def := tools.Definition{
				Name:                 namespaced,
				Version:              version,
				Schema:               tool.InputSchema,
				RequiredCapabilities: slices.Clone(serverCfg.Capabilities),
				Validator:            MakeValidator(tool.InputSchema, int64(serverCfg.MaxMessageSize)),
			}

			rule := approval.Rule{
				ToolName:             namespaced,
				ToolVersion:          version,
				RequiresApproval:     false,
				RequiredCapabilities: slices.Clone(serverCfg.Capabilities),
				AllowedTrust: []approval.TrustLabel{
					approval.TrustOwnerInput,
					approval.TrustTrustedConfiguration,
					approval.TrustDerivedUntrusted,
				},
				Constraints: approval.Constraints{
					MaxOutputBytes: int64(serverCfg.MaxMessageSize),
					Timeout:        time.Duration(serverCfg.RequestTimeout),
				},
			}

			adapter := NewAdapter(client, tool.Name, int64(serverCfg.MaxMessageSize))
			if err := m.broker.RegisterTool(&def, adapter, &rule); err != nil {
				_ = client.Close()
				return Wrap(ErrServerUnavailable, err, "failed to register tool in broker")
			}

			registeredNames = append(registeredNames, namespaced)
		}

		m.clients[serverCfg.Name] = client
		m.registeredTools[serverCfg.Name] = registeredNames

		m.logger.InfoContext(ctx, "server tools registered",
			"component", "mcp_manager",
			"server", serverCfg.Name,
			"tool_count", len(registeredNames),
		)
	}

	return nil
}

func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true

	var errs []error
	for serverName, toolNames := range m.registeredTools {
		for _, name := range toolNames {
			m.broker.UnregisterTool(name, "v1")
		}
		delete(m.registeredTools, serverName)
	}

	for serverName, client := range m.clients {
		if err := client.Close(); err != nil {
			errs = append(errs, err)
		}
		delete(m.clients, serverName)
	}

	return errors.Join(errs...)
}
