package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/toolbroker"
	"github.com/anggasct/aura/internal/tools"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const toolVersion = "v1"

type ManagerOptions struct {
	Config            *config.MCP
	Broker            *toolbroker.Broker
	TrustRegistry     TrustRegistry
	TrustRegistryPath string
	HTTPClient        *http.Client
	EndpointPolicy    EndpointPolicy
	SecretResolver    SecretResolver
	SessionStarter    SessionStarter
	Observer          Observer
	Logger            *slog.Logger
	CapabilityChecker func([]string) error
}

type Manager struct {
	cfg               *config.MCP
	broker            *toolbroker.Broker
	trustRegistry     TrustRegistry
	httpClient        *http.Client
	endpointPolicy    EndpointPolicy
	secretResolver    SecretResolver
	sessionStarter    SessionStarter
	observer          Observer
	logger            *slog.Logger
	capabilityChecker func([]string) error
	clients           map[string]*Client
	registeredTools   map[string][]string
	toolOwners        map[string]string
	failures          map[string]ErrorCode
	customTransports  map[string]sdk.Transport
	mu                sync.Mutex
	closed            bool
}

type ServerStatus struct {
	Name      string
	Transport string
	Code      ErrorCode
	Connected bool
	Tools     []string
}

const (
	startConcurrency       = 4
	maxExposedToolNameSize = 64
	defaultToolSchema      = `{"type":"object"}`
)

func NewManager(opts *ManagerOptions) (*Manager, error) {
	if opts == nil {
		return nil, Errorf(ErrConfigInvalid, "manager options are required")
	}
	if opts.Config == nil {
		return nil, Errorf(ErrConfigInvalid, "mcp configuration is required")
	}
	if opts.Broker == nil {
		return nil, Errorf(ErrConfigInvalid, "tool broker is required")
	}
	registry := opts.TrustRegistry
	if registry == nil {
		if opts.TrustRegistryPath != "" {
			fileRegistry, err := NewFileTrustRegistry(opts.TrustRegistryPath)
			if err != nil {
				return nil, err
			}
			registry = fileRegistry
		} else {
			registry = NewMemoryTrustRegistry()
		}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	return &Manager{
		cfg:               opts.Config,
		broker:            opts.Broker,
		trustRegistry:     registry,
		httpClient:        opts.HTTPClient,
		endpointPolicy:    opts.EndpointPolicy,
		secretResolver:    opts.SecretResolver,
		sessionStarter:    opts.SessionStarter,
		observer:          opts.Observer,
		logger:            opts.Logger,
		capabilityChecker: opts.CapabilityChecker,
		clients:           make(map[string]*Client),
		registeredTools:   make(map[string][]string),
		toolOwners:        make(map[string]string),
		failures:          make(map[string]ErrorCode),
		customTransports:  make(map[string]sdk.Transport),
	}, nil
}

func (m *Manager) SetCustomTransport(serverName string, transport sdk.Transport) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.customTransports[serverName] = transport
}

func (m *Manager) Statuses() []ServerStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	statuses := make([]ServerStatus, 0, len(m.cfg.Servers))
	for i := range m.cfg.Servers {
		server := &m.cfg.Servers[i]
		status := ServerStatus{Name: server.Name, Transport: server.Transport, Code: ErrServerUnavailable}
		if client, running := m.clients[server.Name]; running {
			status.Code = ""
			status.Connected = client.Connected()
			status.Tools = slices.Clone(m.registeredTools[server.Name])
		} else if code, failed := m.failures[server.Name]; failed {
			status.Code = code
		}
		statuses = append(statuses, status)
	}
	return statuses
}

func (m *Manager) UnexposedEntries(entries []string) []string {
	statuses := m.Statuses()
	registered := make(map[string]bool)
	running := make(map[string]bool)
	for i := range statuses {
		if statuses[i].Code != "" {
			continue
		}
		running[statuses[i].Name] = true
		for _, name := range statuses[i].Tools {
			registered[name] = true
		}
	}
	var unexposed []string
	for _, entry := range entries {
		if !tools.IsMCPEntry(entry) || registered[entry] {
			continue
		}
		namesServer, namesTool := false, false
		for server := range running {
			serverEntry := tools.MCPServerEntry(server)
			if entry == serverEntry {
				namesServer = true
			} else if strings.HasPrefix(entry, serverEntry+"_") {
				namesTool = true
			}
		}
		if namesTool && !namesServer {
			unexposed = append(unexposed, entry)
		}
	}
	return unexposed
}

func (m *Manager) Start(ctx context.Context) error {
	if ctx == nil {
		return Errorf(ErrConfigInvalid, "context must not be nil")
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Errorf(ErrServerUnavailable, "manager is closed")
	}
	servers := slices.Clone(m.cfg.Servers)
	transports := maps.Clone(m.customTransports)
	m.mu.Unlock()

	errs := make([]error, len(servers))
	var group errgroup.Group
	group.SetLimit(startConcurrency)
	for i := range servers {
		group.Go(func() error {
			serverCfg := &servers[i]
			err := m.startServer(ctx, serverCfg, transports[serverCfg.Name])
			m.recordOutcome(ctx, serverCfg.Name, err)
			errs[i] = err
			return nil
		})
	}
	_ = group.Wait()
	return errors.Join(errs...)
}

func (m *Manager) recordOutcome(ctx context.Context, serverName string, err error) {
	if err == nil {
		return
	}
	code, ok := CodeOf(err)
	if !ok {
		code = ErrServerUnavailable
	}
	m.mu.Lock()
	m.failures[serverName] = code
	m.mu.Unlock()
	m.logger.WarnContext(ctx, "server start failed",
		"component", "mcp_manager",
		"server", serverName,
		"code", string(code),
	)
}

func (m *Manager) startServer(ctx context.Context, serverCfg *config.MCPServer, transport sdk.Transport) error {
	if m.capabilityChecker != nil && len(serverCfg.Capabilities) > 0 {
		if err := m.capabilityChecker(serverCfg.Capabilities); err != nil {
			return Wrap(ErrCapabilityUnavailable, err, "server "+serverCfg.Name+": required capabilities unavailable")
		}
	}

	configDigest, err := ComputeConfigDigest(serverCfg)
	if err != nil {
		return err
	}
	approved, err := m.trustRegistry.IsConfigTrusted(ctx, serverCfg.Name, configDigest)
	if err != nil {
		return Errorf(ErrTrustRequired, "server %q trust registry is unavailable", serverCfg.Name)
	}
	if !approved {
		return Errorf(ErrTrustRequired, "server %q requires owner approval before it is started", serverCfg.Name)
	}

	client, err := NewClient(serverCfg, m.logger,
		WithHTTPClient(m.httpClient),
		WithEndpointPolicy(m.endpointPolicy),
		WithSecretResolver(m.secretResolver),
		WithSessionStarter(m.sessionStarter),
		WithObserver(m.observer),
	)
	if err != nil {
		return err
	}
	if err := client.Connect(ctx, transport); err != nil {
		_ = client.Close(ctx)
		return err
	}
	discovered, err := client.DiscoverTools(ctx)
	if err != nil {
		_ = client.Close(ctx)
		return err
	}
	if err := m.pinToolSurface(ctx, serverCfg, discovered); err != nil {
		_ = client.Close(ctx)
		return err
	}

	registered, err := m.registerTools(ctx, client, serverCfg, discovered)
	if err != nil {
		_ = client.Close(ctx)
		return err
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.unregister(registered)
		_ = client.Close(ctx)
		return Errorf(ErrServerUnavailable, "manager is closed")
	}
	previous := m.clients[serverCfg.Name]
	previousTools := m.registeredTools[serverCfg.Name]
	m.clients[serverCfg.Name] = client
	m.registeredTools[serverCfg.Name] = registered
	delete(m.failures, serverCfg.Name)
	m.mu.Unlock()
	if previous != nil {
		m.unregisterStale(previousTools, registered)
		_ = previous.Close(ctx)
	}

	m.logger.InfoContext(ctx, "server tools registered",
		"component", "mcp_manager",
		"server", serverCfg.Name,
		"tool_count", len(registered),
	)
	return nil
}

func (m *Manager) pinToolSurface(ctx context.Context, serverCfg *config.MCPServer, discovered []DiscoveredTool) error {
	digest, err := ComputeTrustDigest(serverCfg, discovered)
	if err != nil {
		return err
	}
	trusted, err := m.trustRegistry.IsTrusted(ctx, serverCfg.Name, digest)
	if err != nil {
		return Errorf(ErrTrustRequired, "server %q trust registry is unavailable", serverCfg.Name)
	}
	if trusted {
		return nil
	}
	record, err := m.trustRegistry.GetTrust(ctx, serverCfg.Name)
	if err != nil && !errors.Is(err, ErrTrustNotFound) {
		return Errorf(ErrTrustRequired, "server %q trust registry is unavailable", serverCfg.Name)
	}
	firstDiscovery := err != nil || record.Digest == ""
	toolNames := make([]string, 0, len(discovered))
	for i := range discovered {
		toolNames = append(toolNames, discovered[i].Name)
	}
	if err := m.trustRegistry.SaveSessionTrust(ctx, serverCfg.Name, digest, serverCfg.Capabilities, toolNames); err != nil {
		return Errorf(ErrTrustRequired, "server %q could not record its tool surface", serverCfg.Name)
	}
	if !firstDiscovery {
		return Errorf(ErrTrustRequired, "server %q tool surface changed since approval", serverCfg.Name)
	}
	if err := m.trustRegistry.Approve(ctx, serverCfg.Name, digest); err != nil {
		return Errorf(ErrTrustRequired, "server %q could not pin its tool surface", serverCfg.Name)
	}
	return nil
}

func (m *Manager) registerTools(ctx context.Context, client *Client, serverCfg *config.MCPServer, discovered []DiscoveredTool) ([]string, error) {
	registered := make([]string, 0, len(discovered))
	for i := range discovered {
		tool := &discovered[i]
		namespaced := FormatToolName(serverCfg.Name, tool.Name)
		if len(namespaced) > maxExposedToolNameSize {
			m.logger.WarnContext(ctx, "tool skipped because its exposed name is too long",
				"component", "mcp_manager",
				"server", serverCfg.Name,
				"tool", tool.Name,
			)
			continue
		}
		if err := m.claimToolName(namespaced, serverCfg.Name); err != nil {
			m.unregister(registered)
			return nil, err
		}
		schema := tool.InputSchema
		if !hasToolSchema(schema) {
			schema = json.RawMessage(defaultToolSchema)
		}
		def := tools.Definition{
			Name:                 namespaced,
			Version:              toolVersion,
			Server:               serverCfg.Name,
			Schema:               schema,
			RequiredCapabilities: slices.Clone(serverCfg.Capabilities),
			RequiresApproval:     true,
			Effectful:            true,
			Validator:            MakeValidator(tool.InputSchema, int64(serverCfg.MaxMessageSize)),
		}
		rule := approval.Rule{
			ToolName:             namespaced,
			ToolVersion:          toolVersion,
			RequiresApproval:     true,
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
			m.releaseToolName(namespaced)
			m.unregister(registered)
			return nil, Wrap(ErrServerUnavailable, err, "failed to register tool in broker")
		}
		registered = append(registered, namespaced)
	}
	return registered, nil
}

func (m *Manager) claimToolName(namespaced, serverName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if owner, taken := m.toolOwners[namespaced]; taken && owner != serverName {
		return Errorf(ErrSchemaInvalid, "server %q exposes a tool name already provided by another server", serverName)
	}
	m.toolOwners[namespaced] = serverName
	return nil
}

func (m *Manager) releaseToolName(namespaced string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.toolOwners, namespaced)
}

func (m *Manager) unregister(names []string) {
	for _, name := range names {
		m.broker.UnregisterTool(name, toolVersion)
		m.releaseToolName(name)
	}
}

func (m *Manager) unregisterStale(previous, current []string) {
	for _, name := range previous {
		if slices.Contains(current, name) {
			continue
		}
		m.broker.UnregisterTool(name, toolVersion)
		m.releaseToolName(name)
	}
}

func (m *Manager) Close(ctx context.Context) error {
	if ctx == nil {
		return Errorf(ErrConfigInvalid, "context must not be nil")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true

	var errs []error
	for serverName, toolNames := range m.registeredTools {
		for _, name := range toolNames {
			m.broker.UnregisterTool(name, toolVersion)
			delete(m.toolOwners, name)
		}
		delete(m.registeredTools, serverName)
	}

	for serverName, client := range m.clients {
		if err := client.Close(ctx); err != nil {
			errs = append(errs, err)
		}
		delete(m.clients, serverName)
	}

	return errors.Join(errs...)
}
