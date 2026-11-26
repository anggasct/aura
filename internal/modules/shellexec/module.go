package shellexec

import (
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

type Deps struct {
	Workspace   string
	Config      Config
	Environment []string
}

type Module struct {
	adapter toolbroker.Adapter
}

var _ toolbroker.ToolProvider = (*Module)(nil)

func New(deps *Deps) (*Module, error) {
	if deps == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "shellexec module dependencies must not be nil")
	}
	adapter, err := NewAdapter(Options{
		Workspace:      deps.Workspace,
		Timeout:        time.Duration(deps.Config.Timeout),
		MaxStdoutBytes: deps.Config.MaxStdoutBytes,
		MaxStderrBytes: deps.Config.MaxStderrBytes,
		Environment:    deps.Environment,
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
	return []toolbroker.ToolRegistration{ToolRegistration(m.adapter)}
}
