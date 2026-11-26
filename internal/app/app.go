package app

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"go.opentelemetry.io/otel/metric"

	appconfig "github.com/anggasct/aura/internal/app/config"
	toolsbuiltin "github.com/anggasct/aura/internal/modules/builtin"
	"github.com/anggasct/aura/internal/modules/github"
	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/modules/workflow"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/logging"
	"github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/sandbox"
	"github.com/anggasct/aura/internal/platform/server"
	"github.com/anggasct/aura/internal/platform/telemetry"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeadk "github.com/anggasct/aura/internal/platform/turn/adk"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
	runtimeengine "github.com/anggasct/aura/internal/platform/turn/engine"
	"github.com/anggasct/aura/internal/platform/turn/store"
	"github.com/anggasct/aura/internal/platform/usage"
)

type Surface string

const (
	SurfaceServer  Surface = "server"
	SurfaceChat    Surface = "chat"
	SurfaceCommand Surface = "command"
)

type Part string

const (
	PartDurable   Part = "durable"
	PartWorkflows Part = "workflows"
	PartModels    Part = "models"
	PartKnowledge Part = "knowledge"
)

type Options struct {
	Surface   Surface
	Config    *appconfig.LoadResult
	LogLevel  string
	LogFormat string
	LogOutput io.Writer
	Server    ServerOptions
	Chat      ChatOptions
	Command   CommandOptions
}

type CommandOptions struct {
	Parts []Part
}

type ServerOptions struct {
	Host    sandbox.HostPreparation
	HostErr error
}

type ChatOptions struct {
	In         io.Reader
	Out        io.Writer
	Diag       io.Writer
	SessionID  string
	Persona    string
	Plain      bool
	NoColor    bool
	Interrupts <-chan os.Signal
}

type appState int

const (
	appReady appState = iota
	appRunning
	appRan
	appClosed
)

type App struct {
	surface     Surface
	group       *server.LifecycleGroup
	builtin     *toolsbuiltin.Executor
	serve       func(ctx context.Context) error
	logger      *slog.Logger
	db          *sql.DB
	turns       *turnstore.TurnStore
	result      *appconfig.LoadResult
	runtime     durable.Runtime
	interpreter *workflow.Interpreter
	models      *model.Registry
	knowledge   *knowledge.Service

	mu    sync.Mutex
	state appState
}

func Build(ctx context.Context, opts *Options) (*App, error) {
	if err := validateOptions(opts); err != nil {
		return nil, err
	}
	b := newBuild(opts)
	for _, step := range b.phases(ctx) {
		if err := step(); err != nil {
			if b.group != nil {
				_ = b.group.Stop(ctx)
			}
			return nil, err
		}
	}
	return &App{
		surface: opts.Surface, group: b.group, builtin: b.builtin, serve: b.serve,
		logger: b.logger, db: b.db, turns: b.turns, result: b.result,
		runtime: b.runtime, interpreter: b.interpreter, models: b.models, knowledge: b.knowledge,
	}, nil
}

func (a *App) Run(ctx context.Context) error {
	if a.surface == SurfaceCommand {
		return fault.New(ErrorCodeInvalidArgument, "surface command has no main loop")
	}
	a.mu.Lock()
	if a.state != appReady {
		a.mu.Unlock()
		return fault.New(ErrorCodeInvalidArgument, "app was already run or closed")
	}
	a.state = appRunning
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.state = appRan
		a.mu.Unlock()
	}()
	if err := a.group.Start(ctx); err != nil {
		return err
	}
	return a.serve(ctx)
}

func (a *App) Close(ctx context.Context) error {
	a.mu.Lock()
	if a.state == appRunning {
		a.mu.Unlock()
		return fault.New(ErrorCodeInvalidArgument, "app is still running")
	}
	a.state = appClosed
	a.mu.Unlock()
	return a.group.Stop(ctx)
}

func runAndClose(ctx context.Context, app *App) error {
	return closeAfter(ctx, app, app.Run(ctx))
}

func closeAfter(ctx context.Context, app *App, err error) error {
	closeErr := app.Close(ctx)
	switch {
	case closeErr == nil:
		return err
	case err == nil:
		return closeErr
	default:
		return errors.Join(err, closeErr)
	}
}

type commandInput struct {
	result *appconfig.LoadResult
	parts  []Part
}

func withApp(cmd *cobra.Command, in *commandInput, body func(ctx context.Context, app *App) error) error {
	if in == nil || in.result == nil || in.result.Config == nil {
		return invalidOptions("command input must carry a loaded config")
	}
	ctx := cmd.Context()
	level, format := resolveLogging(cmd, in.result.Config)
	app, err := Build(ctx, &Options{
		Surface:   SurfaceCommand,
		Config:    in.result,
		LogLevel:  level,
		LogFormat: format,
		LogOutput: cmd.ErrOrStderr(),
		Command:   CommandOptions{Parts: in.parts},
	})
	if err != nil {
		return err
	}
	return closeAfter(ctx, app, body(ctx, app))
}

func validateOptions(opts *Options) error {
	switch {
	case opts == nil:
		return invalidOptions("options must not be nil")
	case opts.Config == nil || opts.Config.Config == nil:
		return invalidOptions("config must not be nil")
	case opts.Surface != SurfaceServer && opts.Surface != SurfaceChat && opts.Surface != SurfaceCommand:
		return invalidOptions("surface " + string(opts.Surface) + " is not supported")
	case opts.LogOutput == nil:
		return invalidOptions("log output must not be nil")
	case opts.Surface == SurfaceChat && opts.Chat.In == nil:
		return invalidOptions("chat input must not be nil")
	case opts.Surface == SurfaceChat && opts.Chat.Out == nil:
		return invalidOptions("chat output must not be nil")
	case opts.Surface == SurfaceChat && opts.Chat.Diag == nil:
		return invalidOptions("chat diagnostics output must not be nil")
	case opts.Surface != SurfaceCommand && len(opts.Command.Parts) > 0:
		return invalidOptions("parts are only valid on the command surface")
	}
	return validateParts(opts.Command.Parts)
}

func validateParts(parts []Part) error {
	var seen []Part
	for _, part := range parts {
		switch part {
		case PartDurable, PartWorkflows, PartModels, PartKnowledge:
		default:
			return invalidOptions("part " + string(part) + " is not supported")
		}
		if slices.Contains(seen, part) {
			return invalidOptions("part " + string(part) + " is listed twice")
		}
		seen = append(seen, part)
	}
	return nil
}

func invalidOptions(detail string) error {
	return fault.New(ErrorCodeInvalidArgument, detail)
}

type toolWiring struct {
	observer  toolbroker.Observer
	decider   toolbroker.ApprovalDecider
	access    toolbroker.AccessPolicy
	reachable func(sessionID string) bool
}

type toolExecutorInput struct {
	result       *appconfig.LoadResult
	db           *sql.DB
	artifactRoot string
	logger       *slog.Logger
	wiring       *toolWiring
	events       *turn.EventBus
}

func newToolExecutor(in *toolExecutorInput) (*toolsbuiltin.Executor, error) {
	if in == nil || in.result == nil || in.wiring == nil || in.events == nil {
		return nil, invalidOptions("tool executor input must carry a config result, wiring and the live event bus")
	}
	wiring := in.wiring
	executor, err := toolsbuiltin.New(in.result.Config.Tools, in.result.Config.Storage.ArtifactQuota, in.db, in.artifactRoot, in.result.CapabilityReport, in.logger,
		wiring.observer, wiring.decider, wiring.access,
		toolsbuiltin.WithApprovalReachable(wiring.reachable), toolsbuiltin.WithLiveEvents(in.events))
	if err != nil {
		return nil, err
	}
	for _, value := range githubCredentialSecrets(in.result.Config) {
		executor.Broker().RegisterSecret(value)
	}
	return executor, nil
}

func githubCredentialSecrets(cfg *appconfig.Config) []string {
	if cfg == nil || cfg.Tools == nil {
		return nil
	}
	return cfg.Tools.GitHub.SecretValues()
}

func githubCredentials(cfg *appconfig.Config) []github.Credential {
	if cfg == nil || cfg.Tools == nil {
		return nil
	}
	return cfg.Tools.GitHub.Credentials
}

type lifecycleStage struct {
	name  string
	start func(ctx context.Context) error
	stop  func(ctx context.Context) error
}

func (s *lifecycleStage) Name() string { return s.name }

func (s *lifecycleStage) Start(ctx context.Context) error {
	if s.start == nil {
		return nil
	}
	return s.start(ctx)
}

func (s *lifecycleStage) Stop(ctx context.Context) error {
	if s.stop == nil {
		return nil
	}
	return s.stop(ctx)
}

type build struct {
	opts   *Options
	result *appconfig.LoadResult
	cfg    *appconfig.Config
	logger *slog.Logger
	group  *server.LifecycleGroup

	db           *sql.DB
	turns        *turnstore.TurnStore
	pipeline     *telemetry.Pipeline
	prices       *usage.PriceRegistry
	models       *model.Registry
	runtime      durable.Runtime
	workflowMod  *workflow.Module
	interpreter  *workflow.Interpreter
	knowledge    *knowledge.Service
	quarantined  []workflow.Quarantined
	artifactRoot string

	registry   *auraagent.Registry
	sessions   turnstore.SessionService
	events     turnstore.EventStore
	wiring     toolWiring
	liveEvents *turn.EventBus
	builtin    *toolsbuiltin.Executor
	routeModel func(route string) (string, error)
	ledger     *usage.Ledger

	execOpts []runtimeadk.ExecutorOption
	modules  moduleParts

	executor *runtimeadk.ADKExecutor
	engine   *runtimeengine.Engine

	serve func(ctx context.Context) error

	server *serverBuild
	chat   *chatBuild
}

func (b *build) phases(ctx context.Context) []func() error {
	setup := []func() error{
		func() error { return b.setupLogging(ctx) },
		func() error { return b.openStorage(ctx) },
	}
	if b.opts.Surface == SurfaceCommand {
		return append(setup, func() error { return b.buildCommandParts(ctx) })
	}
	return append(setup,
		b.startTelemetry,
		func() error { return b.buildModelRegistry(ctx) },
		func() error { return b.buildKernel(ctx) },
		func() error { return b.buildModules(ctx) },
		b.buildTurnRuntime,
		func() error { return b.buildSurface(ctx) },
	)
}

func (b *build) buildCommandParts(ctx context.Context) error {
	parts := b.opts.Command.Parts
	if slices.Contains(parts, PartDurable) {
		if _, err := b.durableRuntime(); err != nil {
			return err
		}
	}
	if slices.Contains(parts, PartWorkflows) {
		if err := b.buildWorkflows(ctx, false); err != nil {
			return err
		}
	}
	if slices.Contains(parts, PartModels) {
		if err := b.newModelRegistry(ctx, evaluationModelOptions(b.cfg, b.db)); err != nil {
			return err
		}
	}
	if slices.Contains(parts, PartKnowledge) {
		service, err := newKnowledgeService(b.cfg, b.db, nil)
		if err != nil {
			return err
		}
		b.knowledge = service
	}
	return nil
}

func (b *build) durableRuntime() (durable.Runtime, error) {
	if b.runtime != nil {
		return b.runtime, nil
	}
	runtime, err := durableRuntimeForConfig(b.cfg, b.db, b.logger)
	if err != nil {
		return nil, err
	}
	b.runtime = runtime
	return runtime, nil
}

func newBuild(opts *Options) *build {
	b := &build{opts: opts, result: opts.Config, cfg: opts.Config.Config}
	switch opts.Surface {
	case SurfaceServer:
		b.server = &serverBuild{erasureRuns: &erasureRuns{}}
	case SurfaceChat:
		b.chat = &chatBuild{}
	case SurfaceCommand:
	}
	return b
}

func (b *build) onServer() bool { return b.opts.Surface == SurfaceServer }

func (b *build) primaryModel() string { return b.cfg.Models.Definitions["primary"].Model }

func (b *build) meters() metric.MeterProvider {
	if b.pipeline == nil {
		return nil
	}
	return b.pipeline.MeterProvider()
}

func (b *build) setupLogging(ctx context.Context) error {
	logger, err := logging.Setup(b.opts.LogLevel, b.opts.LogFormat, b.opts.LogOutput)
	if err != nil {
		return err
	}
	b.logger = logger
	b.group = server.NewLifecycleGroup(server.LifecycleOptions{
		StopTimeout: time.Duration(b.cfg.Runtime.ShutdownTimeout),
		Logger:      logger,
	})
	logConfigResult(ctx, logger, b.result)
	switch b.opts.Surface {
	case SurfaceServer:
		logSandboxHost(ctx, logger, b.opts.Server.Host, b.opts.Server.HostErr)
		return b.result.CapabilityStateError
	case SurfaceChat:
		return chatCapabilityGate(ctx, logger, b.result)
	case SurfaceCommand:
	}
	return nil
}

func (b *build) openStorage(ctx context.Context) error {
	db, err := openStorage(ctx, b.cfg)
	if err != nil {
		return err
	}
	b.db = db
	b.turns = turnstore.NewTurnStore(db)
	return b.group.Defer("sqlite", func(context.Context) error {
		_ = db.Close()
		return nil
	})
}

func (b *build) buildModelRegistry(ctx context.Context) error {
	prices, err := openPriceRegistry(ctx, b.logger, b.cfg, b.result.Path, "")
	if err != nil {
		return err
	}
	b.prices = prices
	_, artifactRoot, _, err := storagePaths(b.cfg)
	if err != nil {
		return err
	}
	b.artifactRoot = artifactRoot
	return b.newModelRegistry(ctx, &model.RegistryOptions{
		Logger:     b.logger,
		Models:     b.cfg.Models,
		Routes:     b.cfg.ModelRoutes,
		Checkpoint: model.NewSQLiteCircuitCheckpointStore(b.db),
		Prices:     prices,
		Vision:     visionWiringForConfig(b.cfg, b.db, artifactRoot),
	})
}

func evaluationModelOptions(cfg *appconfig.Config, db *sql.DB) *model.RegistryOptions {
	return &model.RegistryOptions{
		Logger:     slog.New(slog.DiscardHandler),
		Models:     cfg.Models,
		Routes:     cfg.ModelRoutes,
		Checkpoint: model.NewSQLiteCircuitCheckpointStore(db),
	}
}

func (b *build) newModelRegistry(ctx context.Context, opts *model.RegistryOptions) error {
	models, err := model.NewRegistry(ctx, opts)
	if err != nil {
		return err
	}
	b.models = models
	return nil
}

func (b *build) buildKernel(ctx context.Context) error {
	registry, err := buildAgentRegistry(b.cfg)
	if err != nil {
		return err
	}
	b.registry = registry
	b.sessions = turnstore.NewSessionService(b.db)
	b.events = turnstore.NewEventStore(b.db)
	b.liveEvents = turn.NewEventBus(b.logger)
	if b.onServer() {
		logPersonas(ctx, b.logger, registry)
		b.wiring, err = b.serverToolWiring(ctx)
	} else {
		b.wiring, err = b.chatToolWiring(ctx)
	}
	if err != nil {
		return err
	}
	if b.onServer() || b.cfg.Tools != nil {
		builtin, err := newToolExecutor(&toolExecutorInput{
			result: b.result, db: b.db, artifactRoot: b.artifactRoot, logger: b.logger, wiring: &b.wiring,
			events: b.liveEvents,
		})
		if err != nil {
			return err
		}
		b.builtin = builtin
	}
	if b.onServer() {
		if err := b.buildServerTools(ctx); err != nil {
			return err
		}
	}
	routeModel, err := model.RouteModelName(b.cfg.Models, b.cfg.ModelRoutes, b.primaryModel())
	if err != nil {
		return err
	}
	b.routeModel = routeModel
	ledger, err := newTurnLedger(b.cfg, b.db, b.prices, b.logger)
	if err != nil {
		return err
	}
	b.ledger = ledger
	b.execOpts = append(b.execOpts, runtimeadk.WithBudgetLedger(ledger, "primary"))
	if b.builtin != nil {
		b.execOpts = append(b.execOpts, runtimeadk.WithBuiltinToolExecutor(b.builtin))
	}
	b.execOpts = append(b.execOpts,
		runtimeadk.WithAgentResolver(registry, routeModel),
		ownerContextOption(ctx, b.cfg.Owner, b.logger))
	return nil
}

func (b *build) buildTurnRuntime() error {
	if b.liveEvents == nil {
		return invalidOptions("the turn runtime requires the live event bus")
	}
	var broker turn.ToolBroker = terminalBroker{}
	if b.builtin != nil {
		broker = b.builtin
	}
	executor, err := runtimeadk.NewADKExecutor(
		"aura", b.primaryModel(), b.models, b.sessions, broker, nil, b.logger, b.execOpts...,
	)
	if err != nil {
		return err
	}
	b.executor = executor
	hooks := []postTurnHook{b.modules.memoryHook, b.modules.profileHook}
	var durableConfig *runtimeengine.DurableConfig
	var countTurn func(context.Context, *turn.TurnRequest)
	var channels []runtimechannelhost.ChannelPort
	if b.onServer() {
		if err := b.buildServerDurable(); err != nil {
			return err
		}
		durableConfig = b.server.durableConfig
		countTurn = personaTurnCounter(b.server.personaRecorder)
		channels = b.server.channels
	}
	engine, err := runtimeengine.New(&runtimeengine.Deps{
		Events:         b.events,
		Dedupe:         turnstore.NewDedupeStore(b.db),
		Executor:       executor,
		Logger:         b.logger,
		Config:         b.cfg.Runtime,
		DefaultAgentID: auraagent.DefaultID,
		Personas:       registryPersonas{registry: b.registry},
		Skills:         b.modules.skillRenderer,
		Durable:        durableConfig,
		PostTurnHook:   composePostTurnHooks(hooks...),
		CountTurn:      countTurn,
		LiveEvents:     b.liveEvents,
		Channels:       channels,
	})
	if err != nil {
		return err
	}
	b.engine = engine
	return nil
}

func (b *build) buildSurface(ctx context.Context) error {
	if b.onServer() {
		return b.buildServerSurface(ctx)
	}
	return b.buildChatSurface()
}
