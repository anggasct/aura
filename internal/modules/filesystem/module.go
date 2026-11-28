package filesystem

import (
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/workspace"
)

type Deps struct {
	Workspace     string
	MaxFileBytes  int64
	MaxDirEntries int
	TaintTracker  *workspace.Tracker
}

type Module struct {
	adapters map[string]toolbroker.Adapter
}

var _ toolbroker.ToolProvider = (*Module)(nil)

func New(deps *Deps) (*Module, error) {
	if deps == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "filesystem module dependencies must not be nil")
	}
	adapters, err := NewAdapters(Options{
		Workspace:     deps.Workspace,
		MaxFileBytes:  deps.MaxFileBytes,
		MaxDirEntries: deps.MaxDirEntries,
		TaintTracker:  deps.TaintTracker,
	})
	if err != nil {
		return nil, err
	}
	return &Module{adapters: adapters}, nil
}

func (m *Module) Tools() []toolbroker.ToolRegistration {
	if m == nil {
		return nil
	}
	return ToolRegistrations(m.adapters)
}
