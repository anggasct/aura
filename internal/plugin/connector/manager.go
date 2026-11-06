package connector

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/toolbroker"
	"github.com/anggasct/aura/internal/tools"
)

const (
	toolVersion      = "v1"
	startConcurrency = 4
	defaultSchema    = `{"type":"object"}`
)

type SchemaValidator func(schema json.RawMessage, maxBytes int64) func(json.RawMessage) (json.RawMessage, error)

type ManagerOptions struct {
	Plugins           []Config
	Broker            *toolbroker.Broker
	Starter           Starter
	HTTPClient        *http.Client
	Validator         SchemaValidator
	CapabilityChecker func([]string) error
	Logger            *slog.Logger
}

type Status struct {
	ID        string
	Code      ErrorCode
	Connected bool
	Tools     []string
	Drift     []string
}

type entry struct {
	cfg        Config
	host       *host
	registered []string
	drift      []string
	code       ErrorCode
	started    bool
}

type Manager struct {
	broker     *toolbroker.Broker
	starter    Starter
	client     *http.Client
	validator  SchemaValidator
	capability func([]string) error
	logger     *slog.Logger

	mu      sync.Mutex
	entries []*entry
	closed  bool
}

func NewManager(opts *ManagerOptions) (*Manager, error) {
	if opts == nil || opts.Broker == nil || opts.Starter == nil || opts.HTTPClient == nil || opts.Validator == nil {
		return nil, Errorf(ErrConfigInvalid, "a broker, a session starter, an http client and a schema validator are required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	manager := &Manager{
		broker: opts.Broker, starter: opts.Starter, client: opts.HTTPClient, validator: opts.Validator,
		capability: opts.CapabilityChecker, logger: logger,
	}
	seen := make(map[string]bool, len(opts.Plugins))
	for i := range opts.Plugins {
		cfg := &opts.Plugins[i]
		if err := validateConfig(cfg); err != nil || seen[cfg.ID] {
			return nil, Errorf(ErrConfigInvalid, "a connector plugin configuration is not valid")
		}
		seen[cfg.ID] = true
		manager.entries = append(manager.entries, &entry{cfg: cloneConfig(cfg)})
	}
	return manager, nil
}

func validateConfig(cfg *Config) error {
	if !config.ValidPluginID(cfg.ID) || cfg.Executable == "" || cfg.WorkingDir == "" || cfg.Auth == nil {
		return errors.New("identity, executable, working directory and auth are required")
	}
	if cfg.StartupTimeout <= 0 || cfg.RequestTimeout <= 0 || cfg.MaxMessageBytes <= 0 {
		return errors.New("timeouts and the message bound must be positive")
	}
	names := make(map[string]bool, len(cfg.Tools))
	for i := range cfg.Tools {
		if !toolNamePattern.MatchString(cfg.Tools[i].Name) || names[cfg.Tools[i].Name] {
			return errors.New("tool names must be valid and unique")
		}
		names[cfg.Tools[i].Name] = true
	}
	return nil
}

func cloneConfig(cfg *Config) Config {
	clone := *cfg
	clone.Arguments = slices.Clone(cfg.Arguments)
	clone.Capabilities = slices.Clone(cfg.Capabilities)
	clone.Egress = slices.Clone(cfg.Egress)
	clone.Tools = slices.Clone(cfg.Tools)
	return clone
}

func (m *Manager) Start(ctx context.Context) error {
	if ctx == nil {
		return Errorf(ErrConfigInvalid, "a context is required")
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Errorf(ErrBackendUnavailable, "the manager is closed")
	}
	entries := slices.Clone(m.entries)
	m.mu.Unlock()
	errs := make([]error, len(entries))
	var group errgroup.Group
	group.SetLimit(startConcurrency)
	for i := range entries {
		group.Go(func() error {
			if fail := m.startPlugin(ctx, entries[i]); fail != nil {
				errs[i] = fail
			}
			return nil
		})
	}
	_ = group.Wait()
	return errors.Join(errs...)
}

func (m *Manager) startPlugin(ctx context.Context, e *entry) *Error {
	fail := m.bringUp(ctx, e)
	if fail == nil {
		return nil
	}
	m.mu.Lock()
	e.code = ErrBackendUnavailable
	m.mu.Unlock()
	m.logger.WarnContext(ctx, "connector plugin start failed", "component", "plugin_connector", "plugin", e.cfg.ID, "code", string(fail.Code))
	return Wrap(ErrBackendUnavailable, fail, "the plugin could not start")
}

func (m *Manager) bringUp(ctx context.Context, e *entry) *Error {
	if m.capability != nil && len(e.cfg.Capabilities) > 0 {
		if err := m.capability(e.cfg.Capabilities); err != nil {
			return Wrap(ErrBackendUnavailable, err, "a required capability is unavailable")
		}
	}
	runner := newHost(&e.cfg, m.starter, m.client, m.logger)
	described, fail := runner.describe(ctx)
	if fail != nil {
		_ = runner.close(ctx)
		return fail
	}
	exposed, drift := splitApproved(described, e.cfg.Tools)
	registered, err := m.register(runner, &e.cfg, exposed)
	if err != nil {
		_ = runner.close(ctx)
		return Wrap(ErrBackendUnavailable, err, "the tools could not be registered")
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.unregister(registered)
		_ = runner.close(ctx)
		return Errorf(ErrBackendUnavailable, "the manager is closed")
	}
	e.host, e.registered, e.drift, e.started, e.code = runner, registered, drift, true, ""
	m.mu.Unlock()
	m.logger.InfoContext(ctx, "connector tools registered", "component", "plugin_connector", "plugin", e.cfg.ID, "tool_count", len(registered), "hidden_count", len(drift))
	return nil
}

func splitApproved(described []describedTool, approved []Tool) (exposed []describedTool, drift []string) {
	for i := range described {
		if slices.ContainsFunc(approved, func(tool Tool) bool { return tool.Name == described[i].Name }) {
			exposed = append(exposed, described[i])
			continue
		}
		drift = append(drift, described[i].Name)
	}
	return exposed, drift
}

func (m *Manager) register(runner *host, cfg *Config, exposed []describedTool) ([]string, error) {
	registered := make([]string, 0, len(exposed))
	for i := range exposed {
		approved := cfg.Tools[slices.IndexFunc(cfg.Tools, func(tool Tool) bool { return tool.Name == exposed[i].Name })]
		wire := tools.PluginToolWireName(cfg.ID, approved.Name)
		if len(wire) > tools.MaxPluginToolWireLen {
			m.unregister(registered)
			return nil, errors.New("a tool name is too long")
		}
		schema := exposed[i].InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(defaultSchema)
		}
		definition := tools.Definition{
			Name:                 wire,
			Version:              toolVersion,
			Plugin:               &tools.PluginRef{ID: cfg.ID, Description: approved.Description},
			Schema:               schema,
			RequiredCapabilities: slices.Clone(cfg.Capabilities),
			RequiresApproval:     approved.Effect,
			Effectful:            approved.Effect,
			Validator:            m.validator(schema, cfg.MaxMessageBytes),
		}
		rule := approval.Rule{
			ToolName:             wire,
			ToolVersion:          toolVersion,
			RequiresApproval:     approved.Effect,
			RequiredCapabilities: slices.Clone(cfg.Capabilities),
			AllowedTrust: []approval.TrustLabel{
				approval.TrustOwnerInput,
				approval.TrustTrustedConfiguration,
				approval.TrustDerivedUntrusted,
			},
			Constraints: approval.Constraints{MaxOutputBytes: cfg.MaxMessageBytes, Timeout: cfg.RequestTimeout},
		}
		if err := m.broker.RegisterTool(&definition, adapter(runner, &approved), &rule); err != nil {
			m.unregister(registered)
			return nil, err
		}
		registered = append(registered, wire)
	}
	return registered, nil
}

func (m *Manager) unregister(names []string) {
	for _, name := range names {
		m.broker.UnregisterTool(name, toolVersion)
	}
}

func adapter(runner *host, tool *Tool) toolbroker.Adapter {
	return func(ctx context.Context, request *toolbroker.ToolRequest, _ approval.Constraints) (toolbroker.ToolResult, error) {
		output, fail := runner.invoke(ctx, &call{tool: tool, arguments: request.Arguments, idempotencyKey: request.IdempotencyKey})
		if fail != nil {
			return toolbroker.ToolResult{}, toolFailure(tool, fail)
		}
		return toolbroker.ToolResult{
			ToolName: request.ToolName, ToolVersion: request.ToolVersion, Class: toolbroker.ResultOK, Untrusted: true, Output: output,
		}, nil
	}
}

func contextCause(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	}
	return nil
}

func toolFailure(tool *Tool, fail *Error) error {
	cause := contextCause(fail.Err)
	if tool.Effect && fail.RequestSent {
		return &toolbroker.Error{
			Class:  toolbroker.ResultExecutionFailed,
			Detail: "plugin tool outcome is unknown and the request was not retried: " + fail.Public(),
			Cause:  cause,
		}
	}
	failure := &toolbroker.Error{Class: toolbroker.ResultExecutionFailed, Detail: "plugin tool failed: " + fail.Public(), Cause: cause}
	if tool.Effect {
		return toolbroker.NotPerformed(string(fail.Code), failure)
	}
	return failure
}

func (m *Manager) Statuses() []Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	statuses := make([]Status, 0, len(m.entries))
	for _, e := range m.entries {
		status := Status{ID: e.cfg.ID, Code: e.code}
		if e.started {
			status.Connected = e.host.alive.Load()
			status.Tools = slices.Clone(e.registered)
			status.Drift = slices.Clone(e.drift)
		} else if e.code == "" {
			status.Code = ErrBackendUnavailable
		}
		statuses = append(statuses, status)
	}
	return statuses
}

func (m *Manager) Close(ctx context.Context) error {
	if ctx == nil {
		return Errorf(ErrConfigInvalid, "a context is required")
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	type running struct {
		host       *host
		registered []string
	}
	var stop []running
	for _, e := range m.entries {
		if e.started {
			stop = append(stop, running{host: e.host, registered: slices.Clone(e.registered)})
		}
	}
	m.mu.Unlock()
	var errs []error
	for _, item := range stop {
		m.unregister(item.registered)
		if err := item.host.close(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
