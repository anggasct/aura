package fetch

import (
	"time"

	"github.com/anggasct/aura/internal/platform/egress"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

type Deps struct {
	Config   *Config
	Resolver egress.Resolver
}

type Module struct {
	adapter toolbroker.Adapter
}

func New(deps *Deps) (*Module, error) {
	if deps == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "fetch module dependencies must not be nil")
	}
	cfg := Default()
	if deps.Config != nil {
		cfg = *deps.Config
	}
	adapter, err := NewAdapter(Options{
		Timeout:         time.Duration(cfg.Timeout),
		MaxRedirects:    cfg.MaxRedirects,
		MaxEncodedBytes: cfg.MaxEncodedBytes,
		MaxDecodedBytes: cfg.MaxDecodedBytes,
		Resolver:        deps.Resolver,
	})
	if err != nil {
		return nil, err
	}
	return &Module{adapter: adapter}, nil
}

func (m *Module) Tools() []toolbroker.ToolRegistration {
	if m == nil || m.adapter == nil {
		return nil
	}
	return []toolbroker.ToolRegistration{{
		Definition: ToolDefinition(),
		Adapter:    m.adapter,
		Rule:       ToolRule(),
	}}
}

func (m *Module) Adapter() toolbroker.Adapter {
	if m == nil {
		return nil
	}
	return m.adapter
}
