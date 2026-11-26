package skills

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/turn"
)

type ModuleDeps struct {
	Config      *Config
	DB          *sql.DB
	Activations ActivationLog
	Logger      *slog.Logger
}

type Module struct {
	engine   *Engine
	context  *SessionContext
	commands command.Skills
}

func New(ctx context.Context, deps *ModuleDeps) (*Module, error) {
	if ctx == nil {
		return nil, errNilArgument("ctx")
	}
	if deps == nil {
		return nil, errNilArgument("deps")
	}
	if deps.Config == nil {
		return nil, errNilArgument("config")
	}
	if deps.DB == nil {
		return nil, errNilArgument("database")
	}
	if deps.Activations == nil {
		return nil, errNilArgument("activation log")
	}
	roots := make([]string, 0, len(deps.Config.Roots))
	for _, dir := range deps.Config.Roots {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, Errorf(ErrorCodeSkillUnavailable, "skill root %s is not available", filepath.Base(dir))
		}
		roots = append(roots, dir)
	}
	registry, err := NewSQLiteStore(deps.DB)
	if err != nil {
		return nil, err
	}
	engine, err := NewEngine(registry, &EngineConfig{
		Dirs:                 roots,
		MaxIndexed:           deps.Config.MaxIndexedSkills,
		MaxInstructionRunes:  deps.Config.MaxInstructionTokens * CharsPerToken,
		MaxResourceBytes:     deps.Config.MaxResourceBytes,
		ScriptToolName:       "exec",
		ScriptToolCapability: "shell.execute",
		QuarantineRetention:  time.Duration(deps.Config.QuarantineRetention),
		PolicyVersion:        PolicyVersion,
		Logger:               deps.Logger,
	})
	if err != nil {
		return nil, err
	}
	if err := engine.Refresh(ctx); err != nil {
		return nil, err
	}
	sessionContext, err := NewSessionContext(engine, deps.Activations, deps.Config.MaxSessionContextTokens*CharsPerToken)
	if err != nil {
		return nil, err
	}
	var defaultRoot string
	if len(roots) > 0 {
		defaultRoot = roots[0]
	}
	return &Module{
		engine:   engine,
		context:  sessionContext,
		commands: &commandBridge{engine: engine, root: defaultRoot},
	}, nil
}

func (m *Module) TurnContext(ctx context.Context, req *turn.TurnRequest) ([]turn.ContextBlock, error) {
	if m == nil || m.context == nil {
		return nil, nil
	}
	return m.context.TurnContext(ctx, req)
}

func (m *Module) Required(req *turn.TurnRequest) bool {
	if m == nil || m.context == nil {
		return false
	}
	return m.context.Required(req)
}

func (m *Module) Renderer() *SessionContext {
	if m == nil {
		return nil
	}
	return m.context
}

func (m *Module) Engine() *Engine {
	if m == nil {
		return nil
	}
	return m.engine
}

func (m *Module) ChatCommands() command.Skills {
	if m == nil {
		return nil
	}
	return m.commands
}

type commandBridge struct {
	engine *Engine
	root   string
}

func (b *commandBridge) Activate(ctx context.Context, name string) (command.SkillPin, error) {
	activation, err := b.engine.Activate(ctx, name, "explicit: /skill "+name)
	if err != nil {
		return command.SkillPin{}, err
	}
	current, err := b.engine.Current(ctx, activation.SkillID, activation.Digest)
	if err != nil {
		return command.SkillPin{}, err
	}
	if !current {
		return command.SkillPin{}, Errorf(ErrorCodeSkillUnavailable, "skill is not active")
	}
	return command.SkillPin{
		SkillID:       activation.SkillID,
		Digest:        activation.Digest,
		InvocationID:  activation.InvocationID,
		PolicyVersion: activation.PolicyVersion,
	}, nil
}

func (b *commandBridge) StageDraft(ctx context.Context, name, description string) (command.Draft, error) {
	summary, err := b.engine.StageDraft(ctx, name, description, b.root)
	if err != nil {
		return command.Draft{}, err
	}
	return command.Draft{ID: summary.ID, Digest: summary.Digest}, nil
}

var (
	_ turn.ContextProvider = (*Module)(nil)
	_ command.Skills       = (*commandBridge)(nil)
)
