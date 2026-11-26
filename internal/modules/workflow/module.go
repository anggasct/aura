package workflow

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

type ModuleDeps struct {
	DB              *sql.DB
	Sessions        SessionCreator
	RunSessions     RunSessionReader
	Durable         durable.Runtime
	Config          *Config
	ValidationDeps  *ValidationDeps
	Broker          StepBroker
	Registry        *auraagent.Registry
	Route           OwnerRouteHolder
	Targets         []ReplyTargeter
	Offerers        []ProposalOfferer
	Appender        SessionTextAppender
	Deliver         DiscordDeliverer
	CheckTimeout    time.Duration
	ErasureSessions ErasureSessionPort
	ErasureDurable  ErasureDurablePort
	ErasureEffects  ErasureEffectPort
	Logger          *slog.Logger
}

type Module struct {
	store        *Store
	interpreter  *Interpreter
	controller   *Controller
	toolProvider toolbroker.ToolProvider
	completion   *CompletionNotifier
	eraser       *Eraser
	quarantined  []Quarantined
	healthChecks []health.RegisteredCheck
}

func New(ctx context.Context, deps *ModuleDeps) (*Module, error) {
	if deps == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "workflow module dependencies must not be nil")
	}
	if deps.ValidationDeps == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "workflow module requires validation dependencies")
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	runs, err := NewStore(deps.DB, deps.Sessions)
	if err != nil {
		return nil, err
	}
	var definitionsDir string
	maxConcurrent := Default().MaxConcurrentSteps
	var generatedBudget Budget
	if deps.Config != nil {
		definitionsDir = deps.Config.DefinitionsDir
		if deps.Config.MaxConcurrentSteps > 0 {
			maxConcurrent = deps.Config.MaxConcurrentSteps
		}
		generatedBudget = Budget{
			MaxSteps:  deps.Config.Generated.MaxSteps,
			MaxTokens: deps.Config.Generated.MaxTokens,
		}
	}
	specs, err := LoadDefinitionsDir(definitionsDir)
	if err != nil {
		return nil, err
	}
	options := &Options{
		MaxConcurrentSteps: maxConcurrent,
		Logger:             logger,
		GeneratedBudget:    generatedBudget,
	}
	if deps.Broker != nil {
		runner, err := NewToolStepRunner(&ToolStepRunnerDeps{
			Broker:   deps.Broker,
			Sessions: deps.RunSessions,
			Registry: deps.Registry,
			Runs:     runs,
			Route:    deps.Route,
			Logger:   logger,
		})
		if err != nil {
			return nil, err
		}
		options.Tools = runner
	}
	interpreter, err := NewInterpreter(runs, deps.Durable, options)
	if err != nil {
		return nil, err
	}
	routines, err := runs.Routines(ctx)
	if err != nil {
		return nil, err
	}
	if err := interpreter.Install(ctx, specs, deps.ValidationDeps); err != nil {
		return nil, err
	}
	quarantined, err := interpreter.LoadRoutines(ctx, routines, deps.ValidationDeps)
	if err != nil {
		return nil, err
	}
	for i := range quarantined {
		logger.WarnContext(ctx, "workflow routine does not compile and is quarantined", "component", "workflow",
			"definition_id", quarantined[i].ID, "version", quarantined[i].Version, "error", quarantined[i].Err)
	}

	mod := &Module{
		store:       runs,
		interpreter: interpreter,
		quarantined: quarantined,
	}

	if deps.Broker != nil {
		controller, err := NewController(&ControllerDeps{
			Interpreter: interpreter,
			Runs:        runs,
			Registry:    deps.Registry,
			Quarantined: QuarantinedIDs(quarantined),
			Targets:     deps.Targets,
			Offerers:    deps.Offerers,
			ShowCall:    deps.Broker.ExactCallText,
			Logger:      logger,
		})
		if err != nil {
			return nil, err
		}
		provider, err := NewToolProvider(controller)
		if err != nil {
			return nil, err
		}
		mod.controller = controller
		mod.toolProvider = provider
		mod.healthChecks = HealthChecks(deps.Registry, quarantined, deps.CheckTimeout)
		if deps.Appender != nil && deps.Deliver != nil {
			completion, err := NewCompletionNotifier(&CompletionNotifierDeps{
				Runs:     runs,
				Registry: deps.Registry,
				Appender: deps.Appender,
				Deliver:  deps.Deliver,
				Logger:   logger,
			})
			if err != nil {
				return nil, err
			}
			mod.completion = completion
		}
	}

	if deps.ErasureSessions != nil && deps.ErasureDurable != nil && deps.ErasureEffects != nil {
		eraser, err := NewEraser(deps.DB, deps.ErasureSessions, deps.ErasureDurable, deps.ErasureEffects)
		if err != nil {
			return nil, err
		}
		mod.eraser = eraser
	}

	return mod, nil
}

func (m *Module) Tools() []toolbroker.ToolRegistration {
	if m == nil || m.toolProvider == nil {
		return nil
	}
	return m.toolProvider.Tools()
}

func (m *Module) HealthChecks() []health.RegisteredCheck {
	if m == nil {
		return nil
	}
	return m.healthChecks
}

func (m *Module) Interpreter() *Interpreter {
	if m == nil {
		return nil
	}
	return m.interpreter
}

func (m *Module) Store() *Store {
	if m == nil {
		return nil
	}
	return m.store
}

func (m *Module) Controller() *Controller {
	if m == nil {
		return nil
	}
	return m.controller
}

func (m *Module) Eraser() *Eraser {
	if m == nil {
		return nil
	}
	return m.eraser
}

func (m *Module) Quarantined() []Quarantined {
	if m == nil {
		return nil
	}
	return m.quarantined
}

func (m *Module) ProposalCardsRunner() func(context.Context) {
	if m == nil || m.controller == nil {
		return nil
	}
	return m.controller.RunProposalCards
}

func (m *Module) CompletionRunner() func(context.Context) {
	if m == nil || m.completion == nil {
		return nil
	}
	return func(ctx context.Context) {
		m.completion.Run(ctx, DefaultCompletionInterval)
	}
}
