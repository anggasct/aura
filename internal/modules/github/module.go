package github

import (
	"log/slog"
	"slices"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

type ModuleDeps struct {
	Config       *Config
	ToolOptions  *ToolOptions
	Correlations CorrelationStore
	Runtime      durable.Runtime
	Logger       *slog.Logger
}

type Module struct {
	tools   []toolbroker.ToolRegistration
	adapter *Adapter
}

func New(deps *ModuleDeps) (*Module, error) {
	if deps == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "github module dependencies must not be nil")
	}
	var opts ToolOptions
	if deps.ToolOptions != nil {
		opts = *deps.ToolOptions
	}
	if len(opts.Credentials) == 0 && deps.Config != nil && len(deps.Config.Credentials) > 0 {
		opts.Credentials = slices.Clone(deps.Config.Credentials)
	}
	var registrations []toolbroker.ToolRegistration
	if len(opts.Credentials) > 0 {
		provider, err := NewToolProvider(&opts)
		if err != nil {
			return nil, err
		}
		registrations = provider.Tools()
	}
	var adapter *Adapter
	if deps.Correlations != nil || deps.Runtime != nil {
		var err error
		adapter, err = NewAdapter(deps.Correlations, deps.Runtime, deps.Logger)
		if err != nil {
			return nil, err
		}
	}
	return &Module{
		tools:   registrations,
		adapter: adapter,
	}, nil
}

func (m *Module) Tools() []toolbroker.ToolRegistration {
	if m == nil || len(m.tools) == 0 {
		return nil
	}
	return slices.Clone(m.tools)
}

func (m *Module) Adapter() *Adapter {
	if m == nil {
		return nil
	}
	return m.adapter
}
