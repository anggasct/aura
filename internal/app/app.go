package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel/metric"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/channels/email"
	"github.com/anggasct/aura/internal/channels/httpchat"
	"github.com/anggasct/aura/internal/channels/kit/approval"
	"github.com/anggasct/aura/internal/channels/terminal"
	gatewaywebhook "github.com/anggasct/aura/internal/channels/webhook"
	"github.com/anggasct/aura/internal/modules/broadcast"
	"github.com/anggasct/aura/internal/modules/child"
	personaeval "github.com/anggasct/aura/internal/modules/eval/persona"
	"github.com/anggasct/aura/internal/modules/github"
	"github.com/anggasct/aura/internal/modules/handoff"
	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/modules/mcp"
	"github.com/anggasct/aura/internal/modules/memory"
	"github.com/anggasct/aura/internal/modules/plugin"
	"github.com/anggasct/aura/internal/modules/profile"
	"github.com/anggasct/aura/internal/modules/schedule"
	"github.com/anggasct/aura/internal/modules/skills"
	"github.com/anggasct/aura/internal/modules/workflow"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/capability"
	"github.com/anggasct/aura/internal/platform/clikit"
	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/durable"
	durablesqlite "github.com/anggasct/aura/internal/platform/durable/sqlite"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/logging"
	"github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/sandbox"
	"github.com/anggasct/aura/internal/platform/secret"
	"github.com/anggasct/aura/internal/platform/server"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/storage"
	"github.com/anggasct/aura/internal/platform/telemetry"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeadk "github.com/anggasct/aura/internal/platform/turn/adk"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
	runtimeengine "github.com/anggasct/aura/internal/platform/turn/engine"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
	"github.com/anggasct/aura/internal/platform/usage"
	"github.com/anggasct/aura/internal/platform/vision"
	platformwebhook "github.com/anggasct/aura/internal/platform/webhook"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

type BuildMetadata struct {
	Version      string
	Commit       string
	Date         string
	Profile      string
	Capabilities []string
}

func BuildInfo() (BuildMetadata, error) {
	info, err := capability.CurrentBuild()
	if err != nil {
		return BuildMetadata{}, fmt.Errorf("capability: %w", err)
	}
	return BuildMetadata{
		Version:      version,
		Commit:       commit,
		Date:         date,
		Profile:      string(info.Profile()),
		Capabilities: info.CompiledCapabilities(),
	}, nil
}

type ErrorCode string

const (
	ErrorCodeInvalidArgument ErrorCode = "invalid_argument"
	ErrorCodeStopTimeout     ErrorCode = "stop_timeout"
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
	PartMemory    Part = "memory"
	PartProfile   Part = "profile"
	PartPlugins   Part = "plugins"
	PartSkills    Part = "skills"
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

type ChatCommandFlags struct {
	Plain     bool
	SessionID string
	Persona   string
}

type StatusCommandFlags struct {
	AsJSON  bool
	Offline bool
	Probe   string
}

type appState int

const (
	appReady appState = iota
	appRunning
	appRan
	appClosed
)

type App struct {
	surface      Surface
	group        *server.LifecycleGroup
	builtin      *runtimeadk.BrokerExecutor
	serve        func(ctx context.Context) error
	logger       *slog.Logger
	db           *sql.DB
	turns        *turnstore.TurnStore
	result       *appconfig.LoadResult
	runtime      durable.Runtime
	interpreter  *workflow.Interpreter
	models       *model.Registry
	knowledge    *knowledge.Service
	memory       *memory.Service
	profile      *profile.Service
	plugins      *plugin.Service
	skillsEngine *skills.Engine
	skillsRoot   string

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
		runtime: b.runtime, interpreter: b.interpreter, models: b.models, knowledge: b.knowledge, memory: b.memory, profile: b.profile,
		plugins: b.plugins, skillsEngine: b.skillsEngine, skillsRoot: b.skillsRoot,
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

func RunServer(cmd *cobra.Command, gf *GlobalFlags) error {
	if gf == nil {
		gf = &GlobalFlags{}
	}
	ctx := cmd.Context()
	startup, err := prepareHostThenLoad(gf.ConfigPath, sandbox.PrepareHost, appconfig.Load)
	if err != nil {
		return err
	}
	level, format := resolveLogging(cmd, startup.config.Config)
	built, err := Build(ctx, &Options{
		Surface:   SurfaceServer,
		Config:    &startup.config,
		LogLevel:  level,
		LogFormat: format,
		LogOutput: os.Stderr,
		Server:    ServerOptions{Host: startup.host, HostErr: startup.hostErr},
	})
	if err != nil {
		return err
	}
	return runAndClose(ctx, built)
}

func RunChat(cmd *cobra.Command, gf *GlobalFlags, flags ChatCommandFlags) error {
	if gf == nil {
		gf = &GlobalFlags{}
	}
	ctx := cmd.Context()
	result, err := appconfig.Load(gf.ConfigPath)
	if err != nil {
		return err
	}
	level, format := resolveLogging(cmd, result.Config)
	built, err := Build(ctx, &Options{
		Surface:   SurfaceChat,
		Config:    &result,
		LogLevel:  level,
		LogFormat: format,
		LogOutput: os.Stderr,
		Chat: ChatOptions{
			In:         os.Stdin,
			Out:        os.Stdout,
			Diag:       os.Stderr,
			SessionID:  flags.SessionID,
			Persona:    flags.Persona,
			Plain:      flags.Plain,
			NoColor:    os.Getenv("NO_COLOR") != "",
			Interrupts: interruptsFromContext(ctx),
		},
	})
	if err != nil {
		return err
	}
	return runAndClose(ctx, built)
}

func RunStatus(cmd *cobra.Command, gf *GlobalFlags, flags StatusCommandFlags) error {
	return runStatusCommand(cmd, gf, flags, sandbox.Negotiate)
}

func runStatusCommand(cmd *cobra.Command, gf *GlobalFlags, flags StatusCommandFlags, negotiate func() (sandbox.Primitives, error)) error {
	if gf == nil {
		gf = &GlobalFlags{}
	}
	loaded, err := appconfig.Load(gf.ConfigPath)
	if err != nil {
		return clikit.NewExitCodeError(health.ExitCommand, err)
	}
	if flags.Probe != "" {
		if flags.Offline || flags.AsJSON {
			return clikit.NewExitCodeError(health.ExitCommand, errors.New("--probe cannot be combined with --offline or --json"))
		}
		return health.RunProcessProbe(cmd.Context(), cmd.OutOrStdout(), loaded.Config.Health.Listen, flags.Probe)
	}
	return runStatus(cmd, loaded.Config, loaded.CapabilityReport, negotiate, flags.Offline, flags.AsJSON, defaultProcessProbe)
}

func RunDoctor(cmd *cobra.Command, gf *GlobalFlags, checkID string) error {
	return runDoctorCommand(cmd, gf, checkID, sandbox.Negotiate)
}

func runDoctorCommand(cmd *cobra.Command, gf *GlobalFlags, checkID string, negotiate func() (sandbox.Primitives, error)) error {
	if gf == nil {
		gf = &GlobalFlags{}
	}
	ctx := cmd.Context()
	loaded, err := appconfig.Load(gf.ConfigPath)
	if err != nil {
		return clikit.NewExitCodeError(health.ExitCommand, err)
	}
	interpreterChecks, err := interpreterStatusChecks(loaded.Config, loaded.CapabilityReport)
	if err != nil {
		return clikit.NewExitCodeError(health.ExitCommand, err)
	}
	knowledgeChecks, err := knowledgeStatusChecks(loaded.Config)
	if err != nil {
		return clikit.NewExitCodeError(health.ExitCommand, err)
	}
	interpreterChecks = append(interpreterChecks, knowledgeChecks...)
	registry, err := buildHealthRegistry(loaded.Config, mapCapabilityStatuses(loaded.CapabilityReport), localSandboxSupport(negotiate), defaultProcessProbe, interpreterChecks...)
	if err != nil {
		return clikit.NewExitCodeError(health.ExitCommand, err)
	}
	findings := registry.Evaluate(ctx)
	if checkID != "" {
		findings = health.FilterFindingsByCheck(findings, checkID)
		if len(findings) == 0 {
			return clikit.NewExitCodeError(health.ExitCommand, fmt.Errorf("no findings for check %q", checkID))
		}
	}
	out := cmd.OutOrStdout()
	for i := range findings {
		if _, err := fmt.Fprintln(out, health.FormatDoctorFinding(&findings[i])); err != nil {
			return err
		}
	}
	return health.DoctorExit(findings)
}

func ConfigSchema(out io.Writer) error {
	schema, err := appconfig.Schema()
	if err != nil {
		return err
	}
	_, err = out.Write(schema)
	return err
}

func ConfigCheck(cmd *cobra.Command, gf *GlobalFlags) error {
	if gf == nil {
		gf = &GlobalFlags{}
	}
	result, err := appconfig.Load(gf.ConfigPath)
	if err != nil {
		return err
	}
	level, format := resolveLogging(cmd, result.Config)
	logger, err := logging.Setup(level, format, cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	logConfigResult(cmd.Context(), logger, &result)
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "config ok: %s\n", filepath.Base(result.Path))
	return err
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
	built, err := Build(ctx, &Options{
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
	return closeAfter(ctx, built, body(ctx, built))
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
		case PartDurable, PartWorkflows, PartModels, PartKnowledge, PartMemory, PartProfile, PartPlugins, PartSkills:
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

func logConfigResult(ctx context.Context, logger *slog.Logger, result *appconfig.LoadResult) {
	if result.DefaultGenerated {
		logger.InfoContext(ctx, "generating default config", "component", "config", "file", filepath.Base(result.Path))
	}
	for _, key := range result.Warnings {
		logger.WarnContext(ctx, "unrecognized environment variable is ignored", "component", "config", "env", key)
	}
}

func resolveLogging(cmd *cobra.Command, cfg *appconfig.Config) (level, format string) {
	level, format = cfg.Logging.Level, cfg.Logging.Format
	if cmd == nil {
		return level, format
	}
	if f := cmd.Flags().Lookup("log-level"); f != nil && f.Changed {
		level = f.Value.String()
	}
	if f := cmd.Flags().Lookup("log-format"); f != nil && f.Changed {
		format = f.Value.String()
	}
	return level, format
}

func openStorage(ctx context.Context, cfg *appconfig.Config) (*sql.DB, error) {
	return storage.OpenAndMigrate(ctx, &cfg.Storage, Migrations())
}

func openPriceRegistry(ctx context.Context, logger *slog.Logger, cfg *appconfig.Config, configPath, explicitPricesPath string) (*usage.PriceRegistry, error) {
	reg := usage.NewPriceRegistry()
	if cfg != nil {
		for name := range cfg.Models.Definitions {
			def := cfg.Models.Definitions[name]
			if def.Capabilities.MicrosPerInputToken > 0 || def.Capabilities.MicrosPerOutputToken > 0 {
				if err := reg.Put(&usage.Price{
					ModelDefinitionID:    name,
					Currency:             "USD",
					MicrosPerInputToken:  def.Capabilities.MicrosPerInputToken,
					MicrosPerOutputToken: def.Capabilities.MicrosPerOutputToken,
					EffectiveFrom:        time.Unix(0, 0),
					MaxReservationRate:   100,
					Source:               "config",
				}); err != nil {
					return nil, err
				}
			}
		}
	}
	if err := usage.LoadPrices(ctx, logger, configPath, explicitPricesPath, reg); err != nil {
		return nil, err
	}
	return reg, nil
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
	clock        durable.Clock
}

func newToolBroker(in *toolExecutorInput) (*toolbroker.Broker, *runtimeadk.BrokerExecutor, storage.ArtifactStore, error) {
	if in == nil || in.result == nil || in.result.Config == nil || in.wiring == nil {
		return nil, nil, nil, invalidOptions("tool executor input must carry a config result and wiring")
	}
	if in.events == nil {
		return nil, nil, nil, fault.New(turn.ErrorCodeInvalidArgument, "live events publisher must not be nil")
	}
	if in.result.Config.Tools == nil {
		return nil, nil, nil, errors.New("tools configuration is required")
	}
	if in.db == nil {
		return nil, nil, nil, errors.New("storage database is required")
	}
	if in.artifactRoot == "" {
		return nil, nil, nil, errors.New("artifact root is required")
	}
	logger := in.logger
	if logger == nil {
		logger = slog.Default()
	}
	journal, err := effect.NewJournal(in.db, effect.Options{Logger: logger, Events: in.events, Turns: turnstore.NewTurnStore(in.db)})
	if err != nil {
		return nil, nil, nil, err
	}
	effects, err := effect.NewExecutor(journal)
	if err != nil {
		return nil, nil, nil, err
	}
	host := in.result.CapabilityReport.Active()
	artifacts := storage.NewArtifactStore(in.db, in.artifactRoot, int64(in.result.Config.Storage.ArtifactQuota))
	wiring := in.wiring
	broker, err := toolbroker.New(&toolbroker.Options{
		Secrets:              configuredToolSecrets(in.result.Config.Tools),
		MaxInlineResultBytes: in.result.Config.Tools.MaxInlineResultBytes,
		Artifacts:            artifacts,
		Effects:              effects,
		Observer:             wiring.observer,
		ApprovalDecider:      wiring.decider,
		Access:               wiring.access,
		HostCapabilities:     host,
		ApprovalReachable:    wiring.reachable,
		Clock:                in.clock,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	for _, value := range githubCredentialSecrets(in.result.Config) {
		broker.RegisterSecret(value)
	}
	executor, err := runtimeadk.NewBrokerExecutor(broker, host)
	if err != nil {
		return nil, nil, nil, err
	}
	return broker, executor, artifacts, nil
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

	db             *sql.DB
	turns          *turnstore.TurnStore
	pipeline       *telemetry.Pipeline
	prices         *usage.PriceRegistry
	models         *model.Registry
	runtime        durable.Runtime
	workflowMod    *workflow.Module
	personaEvalMod *personaeval.Module
	interpreter    *workflow.Interpreter
	knowledge      *knowledge.Service
	memory         *memory.Service
	profile        *profile.Service
	plugins        *plugin.Service
	skillsEngine   *skills.Engine
	skillsRoot     string
	quarantined    []workflow.Quarantined
	artifactRoot   string

	registry   *auraagent.Registry
	sessions   turnstore.SessionService
	events     turnstore.EventStore
	wiring     toolWiring
	liveEvents *turn.EventBus
	broker     *toolbroker.Broker
	clock      durable.Clock
	bindings   []operation.Binding
	behaviors  []operation.Behavior
	artifacts  storage.ArtifactStore
	builtin    *runtimeadk.BrokerExecutor
	routeModel func(route string) (string, error)
	ledger     *usage.Ledger

	execOpts  []runtimeadk.ExecutorOption
	turnHooks []turn.TurnHook
	modules   moduleParts

	executor *runtimeadk.ADKExecutor

	operationPort *runtimeadk.OperationPort
	engine        *runtimeengine.Engine

	serve func(ctx context.Context) error

	server *serverBuild
	chat   *chatBuild
}

type serverBuild struct {
	channels           []runtimechannelhost.ChannelPort
	checks             []health.RegisteredCheck
	approvalAdapters   []approvalAdapter
	emailMailboxes     []*email.Adapter
	ownerRoute         *approval.OwnerRoute
	workflowRoute      *approval.OwnerRoute
	accessRecorder     *telemetry.AccessRecorder
	commandTurns       *deferredCommandTurns
	commandSkills      *deferredCommandSkills
	mcp                *mcp.Module
	personaRecorder    *telemetry.PersonaRecorder
	durableConfig      *runtimeengine.DurableConfig
	host               *runtimeengine.Host
	broadcastRuntime   durable.Runtime
	hostedHandlers     []string
	broadcastMod       *broadcast.Module
	scheduleMod        *schedule.Module
	scheduledWorkflows *scheduleWorkflows
	erasureRuns        *erasureRuns
	childLedger        child.BudgetLedger
	listeners          *server.Server
	httpChat           *httpchat.Server
}

type chatPresentation struct {
	plain   bool
	noColor bool
}

func shouldUseTTY(present chatPresentation, inTTY, outTTY bool) bool {
	if present.plain {
		return false
	}
	return inTTY && outTTY
}

type chatBuild struct {
	inFile    *os.File
	outFile   *os.File
	useTTY    bool
	approvals *terminal.ApprovalBridge
	agentID   string
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
	if slices.Contains(parts, PartMemory) {
		if !b.cfg.Memory.Enabled {
			return errors.New("memory is not enabled")
		}
		memStore, err := memory.NewSQLiteStore(b.db, turnstore.NewSessionService(b.db))
		if err != nil {
			return err
		}
		service, err := memory.NewService(memStore, secret.New(), &memory.ServiceOptions{
			MaxDocuments:         b.cfg.Memory.MaxDocuments,
			TokenBudget:          b.cfg.Memory.RecallTokenBudget,
			SummaryPromptVersion: b.cfg.Memory.SummaryPromptVersion,
			SummaryTTL:           time.Duration(b.cfg.Memory.SummaryTTL),
			RangeThreshold:       b.cfg.Memory.RangeThreshold,
			RangeLiveWindow:      b.cfg.Memory.RangeLiveWindow,
		})
		if err != nil {
			return err
		}
		b.memory = service
	}
	if slices.Contains(parts, PartProfile) {
		store, err := profile.NewSQLiteStore(b.db)
		if err != nil {
			return err
		}
		sink, err := profile.NewActionSink(turnstore.NewSessionService(b.db), turnstore.NewEventStore(b.db))
		if err != nil {
			return err
		}
		minConfidence := profile.Default().CandidateMinConfidence
		if b.cfg.Profile != nil {
			minConfidence = b.cfg.Profile.CandidateMinConfidence
		}
		service, err := profile.NewService(store, profile.ServiceOptions{
			MinConfidence: minConfidence,
			Actions:       sink,
		})
		if err != nil {
			return err
		}
		b.profile = service
	}
	if slices.Contains(parts, PartPlugins) {
		service, err := openPluginService(b.cfg, b.db, discardRedactor{})
		if err != nil {
			return err
		}
		b.plugins = service
	}
	if slices.Contains(parts, PartSkills) {
		if b.cfg.Skills == nil || !b.cfg.Skills.Enabled {
			return errors.New("skills are not enabled")
		}
		mod, err := skills.New(ctx, &skills.ModuleDeps{
			Config:      b.cfg.Skills,
			DB:          b.db,
			Activations: &skillActivationLog{events: turnstore.NewEventKindReader(b.db)},
			Logger:      b.logger,
		})
		if err != nil {
			return err
		}
		b.skillsEngine = mod.Engine()
		if len(b.cfg.Skills.Roots) > 0 {
			b.skillsRoot = b.cfg.Skills.Roots[0]
		}
	}
	return nil
}

type runHandlerDeclarer interface {
	DeclareHandlers(names ...string)
}

func declareRunHandlers(runtime runHandlerDeclarer) {
	runtime.DeclareHandlers(broadcast.HandlerName, schedule.HandlerName, child.HandlerName, workflow.HandlerName)
}

func durableRuntimeForConfig(cfg *appconfig.Config, db *sql.DB, logger *slog.Logger) (durable.Runtime, error) {
	if cfg == nil || db == nil {
		return nil, invalidOptions("the durable runtime needs a config and open storage")
	}
	runtime, err := durablesqlite.New(durablesqlite.HostOptions(db, logger, time.Duration(cfg.Runtime.ShutdownTimeout)))
	if err != nil {
		return nil, err
	}
	declareRunHandlers(runtime)
	return runtime, nil
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
	b := &build{opts: opts, result: opts.Config, cfg: opts.Config.Config, clock: durable.RealClock()}
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

func (b *build) repliesToRequester(toolName string) bool {
	if b.broker == nil {
		return false
	}
	definition, ok := b.broker.Definition(toolName, tools.Version)
	return ok && definition.RepliesToRequester
}

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

func chatCapabilityGate(ctx context.Context, logger *slog.Logger, result *appconfig.LoadResult) error {
	if result.CapabilityStateError == nil {
		return nil
	}
	if !capability.IsMissingDependency(result.CapabilityStateError) {
		return result.CapabilityStateError
	}
	logger.WarnContext(ctx, "capability dependency is missing; tools that need it are not offered",
		"component", "chat",
		"error", result.CapabilityStateError,
	)
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
		return db.Close()
	})
}

func (b *build) startTelemetry() error {
	if !b.onServer() {
		return nil
	}
	pipeline, err := telemetry.NewPipeline(b.cfg.Telemetry, b.logger)
	if err != nil {
		return err
	}
	b.pipeline = pipeline
	logger := b.logger
	return b.group.Defer("telemetry", func(ctx context.Context) error {
		if err := pipeline.Shutdown(ctx); err != nil {
			logger.WarnContext(ctx, "telemetry shutdown", "component", "telemetry", "error", err)
		}
		return nil
	})
}

func (b *build) buildModelRegistry(ctx context.Context) error {
	prices, err := openPriceRegistry(ctx, b.logger, b.cfg, b.result.Path, "")
	if err != nil {
		return err
	}
	b.prices = prices
	_, artifactRoot, _, err := storage.ResolvePaths(&b.cfg.Storage)
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

func visionWiringForConfig(cfg *appconfig.Config, db *sql.DB, artifactRoot string) model.VisionWiring {
	if cfg == nil || db == nil {
		return model.VisionWiring{}
	}
	policy := model.NewVisionPolicy(cfg.Vision)
	if policy == nil {
		return model.VisionWiring{}
	}
	if strings.TrimSpace(artifactRoot) == "" {
		return model.VisionWiring{}
	}
	blobs := storage.NewArtifactStore(db, artifactRoot, int64(cfg.Storage.ArtifactQuota))
	return model.VisionWiring{
		Policy: policy,
		Blobs: vision.NewBlobReader(func(ctx context.Context, refID string) (io.ReadCloser, error) {
			stream, _, err := blobs.Open(ctx, refID)
			return stream, err
		}),
	}
}

func newVisionService(cfg *appconfig.Config, db *sql.DB, artifactRoot string) *vision.Service {
	if cfg == nil || cfg.Vision == nil || !cfg.Vision.Enabled {
		return nil
	}
	if db == nil || strings.TrimSpace(artifactRoot) == "" {
		return nil
	}
	limits := vision.LimitsFromConfig(cfg.Vision)
	stores := storage.NewVisionStore(storage.NewArtifactStore(db, artifactRoot, int64(cfg.Storage.ArtifactQuota)))
	opts := []vision.Option{vision.WithStore(stores)}
	if recorder, err := telemetry.NewVisionRecorder(nil); err == nil && recorder != nil {
		opts = append(opts, vision.WithObserver(recorder.VisionObserver()))
	}
	svc, err := vision.NewService(&limits, opts...)
	if err != nil {
		return nil
	}
	return svc
}

const ownerContextWarnBytes = 4096

func ownerContextOption(ctx context.Context, owner *principal.Owner, logger *slog.Logger) runtimeadk.ExecutorOption {
	if logger == nil {
		logger = slog.Default()
	}
	if owner == nil {
		return runtimeadk.WithOwnerContext("")
	}
	block := runtimeadk.RenderOwnerContext(&runtimeadk.OwnerContext{
		Name:        owner.Name,
		Language:    owner.Language,
		Timezone:    owner.Timezone,
		Preferences: owner.Preferences,
	})
	if block == "" {
		return runtimeadk.WithOwnerContext("")
	}
	logger.InfoContext(ctx, "owner context attached",
		"component", "owner",
		"digest", runtimeadk.OwnerContextDigest(block),
		"bytes", len(block),
	)
	if len(block) > ownerContextWarnBytes {
		logger.WarnContext(ctx, "owner context exceeds the size guideline",
			"component", "owner",
			"bytes", len(block),
			"limit", ownerContextWarnBytes,
		)
	}
	return runtimeadk.WithOwnerContext(block)
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
		auraagent.LogPersonas(ctx, b.logger, registry)
		b.wiring, err = b.serverToolWiring(ctx)
	} else {
		b.wiring, err = b.chatToolWiring(ctx)
	}
	if err != nil {
		return err
	}
	if b.onServer() || b.cfg.Tools != nil {
		broker, builtin, artifacts, err := newToolBroker(&toolExecutorInput{
			result: b.result, db: b.db, artifactRoot: b.artifactRoot, logger: b.logger, wiring: &b.wiring,
			events: b.liveEvents, clock: b.clock,
		})
		if err != nil {
			return err
		}
		b.broker = broker
		b.builtin = builtin
		b.artifacts = artifacts
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
	ledger, err := usage.NewTurnLedger(b.cfg.Usage, b.cfg.Access, b.db, b.prices, b.logger)
	if err != nil {
		return err
	}
	b.ledger = ledger
	b.execOpts = append(b.execOpts,
		runtimeadk.WithBudgetLedger(ledger, "primary"),
		runtimeadk.WithAgentResolver(registry, routeModel),
		ownerContextOption(ctx, b.cfg.Owner, b.logger))
	return nil
}

func toolObserver(recorder *telemetry.ToolRecorder) toolbroker.Observer {
	return func(ctx context.Context, observation toolbroker.Observation) {
		observed := telemetry.ToolObservation{
			Name:          observation.ToolName + "@" + observation.ToolVersion,
			Status:        string(observation.Class),
			PolicyOutcome: observation.PolicyOutcome,
			Approval:      observation.Approval,
			Executor:      observation.Executor,
			Duration:      observation.Duration,
			OutputBytes:   observation.OutputBytes,
			OperationID:   observation.OperationID,
		}
		if call, ok := workflow.StepCallFrom(ctx); ok {
			observed.WorkflowRunID, observed.WorkflowStepID = call.RunID, call.StepID
		}
		recorder.Record(ctx, &observed)
	}
}

func (b *build) serverToolWiring(ctx context.Context) (toolWiring, error) {
	cfg, db, s, meters := b.cfg, b.db, b.server, b.meters()
	toolRecorder, err := telemetry.NewToolRecorder(b.pipeline.TracerProvider(), meters)
	if err != nil {
		return toolWiring{}, err
	}
	personaRecorder, err := telemetry.NewPersonaRecorder(meters)
	if err != nil {
		return toolWiring{}, err
	}
	s.personaRecorder = personaRecorder
	personaRouter, err := auraagent.NewRouter(b.registry, channelAccounts(&cfg.Channels), cfg.Bindings)
	if err != nil {
		return toolWiring{}, err
	}
	emailRetry, err := recoverEmailEffects(ctx, cfg, db, b.logger)
	if err != nil {
		return toolWiring{}, err
	}
	if emailRetry.pending() {
		if err := b.group.Add(server.Background("email_effect_recovery", emailRetry.run)); err != nil {
			return toolWiring{}, err
		}
	}
	commandRecorder, err := telemetry.NewCommandRecorder(b.pipeline.TracerProvider(), meters)
	if err != nil {
		return toolWiring{}, err
	}
	s.commandTurns = &deferredCommandTurns{}
	s.commandSkills = &deferredCommandSkills{}
	dispatcher, err := newChatCommandDispatcher(b.sessions, b.events, turnstore.NewConversationSessionStore(db), s.commandTurns, s.commandSkills, commandRecorder)
	if err != nil {
		return toolWiring{}, err
	}
	built, err := buildServerChannels(cfg, personaRouter, db, b.artifactRoot, meters, s.erasureRuns, dispatcher, b.logger)
	if err != nil {
		return toolWiring{}, err
	}
	s.channels, s.checks, s.approvalAdapters = built.ports, built.checks, built.approvers
	s.ownerRoute, s.workflowRoute, s.accessRecorder = built.ownerRoute, built.workflowRoute, built.accessRecorder
	s.emailMailboxes = built.emailMailboxes
	approvalTargets := approvalChannels(built.approvers, built.ownerRoute, built.workflowRoute)
	var onGrantUsed func(context.Context)
	if built.accessRecorder != nil {
		onGrantUsed = built.accessRecorder.StandingGrantUsed
	}
	return toolWiring{
		observer:  toolObserver(toolRecorder),
		decider:   approval.Decider(approvalTargets),
		access:    auraagent.NewAccessPolicy(b.registry, principal.NewStandingGrantStore(db), onGrantUsed),
		reachable: approval.Reachable(approvalTargets),
	}, nil
}

func (b *build) buildServerTools(ctx context.Context) error {
	cfg, db, s := b.cfg, b.db, b.server
	notifiers := handoffNotifiers(s.channels)
	if cfg.Channels.HTTP.Enabled {
		httpNotifier, err := newHTTPHandoffNotifier(s.ownerRoute, s.approvalAdapters)
		if err != nil {
			return err
		}
		notifiers = append(notifiers, httpNotifier)
	}
	var handoffTimeout time.Duration
	if cfg.Access != nil {
		handoffTimeout = time.Duration(cfg.Access.HandoffTimeout)
	}
	handoffMod, err := handoff.New(&handoff.ModuleDeps{
		DB:        db,
		Notifiers: notifiers,
		Timeout:   handoffTimeout,
	})
	if err != nil {
		return err
	}
	if err := b.registerModule("handoff", handoffMod); err != nil {
		return err
	}
	if err := b.broker.RegisterProvider(email.NewToolProvider(s.emailMailboxes...)); err != nil {
		return err
	}
	pluginMod, err := buildPluginModule(ctx, cfg, db, b.broker, b.result.CapabilityReport, b.logger)
	if err != nil {
		return err
	}
	if err := b.registerModule("plugin", pluginMod); err != nil {
		return err
	}
	pluginMod.WarnUnavailable(ctx, b.registry.Definitions())
	if pluginMod.HasConnectors() {
		logger := b.logger
		if err := b.group.Defer("plugin_connector", func(ctx context.Context) error {
			if err := pluginMod.Close(ctx); err != nil {
				logger.WarnContext(ctx, "connector plugin shutdown", "component", "plugin_connector", "error", err)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if err := b.startMCPServers(ctx, pluginMod); err != nil {
		return err
	}
	pluginCheck, pluginChecked, err := pluginHealthCheck(pluginMod, s.mcp, time.Duration(cfg.Health.CheckTimeout))
	if err != nil {
		return err
	}
	if pluginChecked {
		b.addHealthCheck(&pluginCheck)
	}
	if b.primaryModel() == "" {
		return configkit.Invalid("models.definitions.primary.model", "is required for the server runtime")
	}
	return nil
}

func (b *build) startMCPServers(ctx context.Context, plugins *plugin.Module) error {
	cfg, logger := b.cfg, b.logger
	mcpPlugins := mcpPluginsFromBackends(plugins.MCPBackends())
	if len(configuredMCPServers(cfg)) == 0 && len(mcpPlugins) == 0 {
		return nil
	}
	trustPath, err := mcpTrustRegistryPath(cfg)
	if err != nil {
		return err
	}
	mod, err := mcp.New(ctx, &mcp.ModuleDeps{
		Config:       cfg.MCP,
		Plugins:      mcpPlugins,
		Broker:       b.broker,
		TrustPath:    trustPath,
		Meters:       b.meters(),
		Capabilities: b.result.CapabilityReport,
		Personas:     b.registry.Definitions(),
		CheckTimeout: time.Duration(cfg.Health.CheckTimeout),
		Logger:       logger,
	})
	if err != nil {
		return err
	}
	b.server.mcp = mod
	if err := b.registerModule("mcp", mod); err != nil {
		_ = mod.Close(context.WithoutCancel(ctx))
		return err
	}
	return b.group.Defer("mcp", func(ctx context.Context) error {
		if err := mod.Close(ctx); err != nil {
			logger.WarnContext(ctx, "mcp shutdown", "component", "mcp", "error", err)
		}
		return nil
	})
}

func (b *build) buildServerDurable() error {
	runtime, err := b.durableRuntime()
	if err != nil {
		return err
	}
	sessions, err := durablesqlite.NewSessionStore(&durablesqlite.SessionStoreOptions{DB: b.db, Turns: b.turns})
	if err != nil {
		return err
	}
	b.server.durableConfig = &runtimeengine.DurableConfig{Sessions: sessions, Runtime: runtime}
	b.server.hostedHandlers = append(b.server.hostedHandlers, runtimeengine.HandlerName)
	return nil
}

func (b *build) buildTurnRuntime() error {
	if b.liveEvents == nil {
		return invalidOptions("the turn runtime requires the live event bus")
	}
	var broker turn.ToolBroker = terminal.DenyPolicy{}
	b.operationPort = runtimeadk.NewOperationPort()
	execOpts := append([]runtimeadk.ExecutorOption{runtimeadk.WithToolBudget(toolBudgetFromRuntime(b.cfg.Runtime)), runtimeadk.WithOperations(b.operationPort)}, b.execOpts...)
	if b.builtin != nil {
		broker = b.builtin
		if len(b.builtin.Definitions()) > 0 {
			execOpts = append(execOpts, runtimeadk.WithBuiltinToolExecutor(b.builtin))
		}
	}
	executor, err := runtimeadk.NewADKExecutor(
		"aura", b.primaryModel(), b.models, b.sessions, broker, nil, b.logger, execOpts...,
	)
	if err != nil {
		return err
	}
	b.executor = executor
	hooks := slices.Clone(b.turnHooks)
	var durableConfig *runtimeengine.DurableConfig
	var countTurn func(context.Context, *turn.TurnRequest)
	var channels []runtimechannelhost.ChannelPort
	if b.onServer() {
		if err := b.buildServerDurable(); err != nil {
			return err
		}
		durableConfig = b.server.durableConfig
		recorder := b.server.personaRecorder
		countTurn = func(ctx context.Context, req *turn.TurnRequest) {
			recorder.Turn(ctx, req.AgentID)
		}
		channels = b.server.channels
	}
	engine, err := runtimeengine.New(&runtimeengine.Deps{
		Events:             b.events,
		Dedupe:             turnstore.NewDedupeStore(b.db),
		Executor:           executor,
		Logger:             b.logger,
		Config:             b.cfg.Runtime,
		DefaultAgentID:     auraagent.DefaultID,
		Personas:           auraagent.RegistryPersonas{Registry: b.registry},
		RepliesToRequester: b.repliesToRequester,
		Skills:             b.modules.skillRenderer,
		Durable:            durableConfig,
		PostTurnHook:       composeTurnHooks(hooks),
		CountTurn:          countTurn,
		LiveEvents:         b.liveEvents,
		Channels:           channels,
	})
	if err != nil {
		return err
	}
	b.engine = engine
	if b.onServer() && b.server.commandTurns != nil {
		b.server.commandTurns.bind(engine)
	}
	return nil
}

func (b *build) buildSurface(ctx context.Context) error {
	if b.onServer() {
		return b.buildServerSurface(ctx)
	}
	return b.buildChatSurface()
}

func (b *build) buildServerSurface(ctx context.Context) error {
	steps := []func() error{
		b.buildChannelHost,
		b.buildBroadcast,
		b.buildSchedule,
		b.buildOperations,
		func() error { return b.buildWorkflowHost(ctx) },
		b.buildDurableHost,
		b.buildProbe,
		b.wireWorkflowParts,
		b.bindErasureRuns,
		b.addEdgeListeners,
		b.registerSweeps,
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

func (b *build) buildChannelHost() error {
	var err error
	b.server.host, err = runtimeengine.NewHost(b.engine, b.logger)
	return err
}

func (b *build) buildBroadcast() error {
	cfg, db, logger, s := b.cfg, b.db, b.logger, b.server
	broadcastEffects, err := newChannelEffects(db, logger)
	if err != nil {
		return err
	}
	senders := buildBroadcastSenders(s.channels, broadcastEffects, b.sessions)
	broadcastRecorder, err := telemetry.NewBroadcastRecorder(b.meters())
	if err != nil {
		return err
	}
	s.broadcastRuntime, err = b.durableRuntime()
	if err != nil {
		return err
	}
	s.broadcastMod, err = broadcast.New(&broadcast.ModuleDeps{
		Config:   cfg.Broadcast,
		DB:       db,
		Senders:  senders,
		Runtime:  s.broadcastRuntime,
		Logger:   logger,
		Observer: broadcastRecorderObserver(broadcastRecorder),
	})
	if err != nil {
		return err
	}
	registrar, ok := s.broadcastRuntime.(durable.HandlerRegistrar)
	if !ok || registrar == nil {
		return errors.New("broadcast handler target does not accept handlers")
	}
	registrar.RegisterHandler(broadcast.HandlerName, s.broadcastMod.Handle)
	s.hostedHandlers = append(s.hostedHandlers, broadcast.HandlerName)
	broadcastMod := s.broadcastMod
	return b.group.Add(&lifecycleStage{name: "broadcast_resume", start: func(ctx context.Context) error {
		resumed, err := broadcastMod.Resume(ctx)
		if err != nil {
			return err
		}
		if resumed > 0 {
			logger.InfoContext(ctx, "resumed broadcast runs", "component", "broadcast", "count", resumed)
		}
		return nil
	}})
}

func (b *build) buildSchedule() error {
	cfg, db, logger, s := b.cfg, b.db, b.logger, b.server
	scheduleRecorder, err := telemetry.NewScheduleRecorder(b.meters())
	if err != nil {
		return err
	}
	s.scheduledWorkflows = &scheduleWorkflows{}
	s.scheduleMod, err = schedule.New(&schedule.ModuleDeps{
		Config:   cfg.Scheduler,
		DB:       db,
		Turns:    &engineTurnRunner{engine: b.engine, sessions: b.sessions, workflows: s.scheduledWorkflows},
		Notify:   &cronNotifier{broadcaster: s.broadcastMod.Broadcaster()},
		Starter:  s.broadcastRuntime,
		Logger:   logger,
		Observer: scheduleRecorderObserver(scheduleRecorder),

		WorkflowChecker: func(ctx context.Context, id string) error {
			return checkScheduledWorkflow(ctx, cfg, db, id)
		},
		Owner: &cfg.Broadcast,
	})
	if err != nil {
		return err
	}
	if err := b.registerModule("schedule", s.scheduleMod); err != nil {
		return err
	}
	registrar, ok := s.broadcastRuntime.(durable.HandlerRegistrar)
	if !ok || registrar == nil {
		return errors.New("schedule handler target does not accept handlers")
	}
	registrar.RegisterHandler(schedule.HandlerName, s.scheduleMod.Handle)
	s.hostedHandlers = append(s.hostedHandlers, schedule.HandlerName)
	s.childLedger = child.NewLedger(nil, 0)
	if cfg.Children != nil && cfg.Children.Enabled {
		var turns child.TurnRunner
		if b.executor != nil {
			turns = b.executor
		}
		childMod, err := child.New(&child.ModuleDeps{
			DB:        db,
			Sessions:  b.sessions,
			Turns:     turns,
			Runtime:   s.broadcastRuntime,
			Ledger:    s.childLedger,
			ModelName: b.primaryModel(),
		})
		if err != nil {
			return err
		}
		if err := childMod.Register(registrar); err != nil {
			return err
		}
		s.hostedHandlers = append(s.hostedHandlers, child.HandlerName)
	}
	scheduleMod, host := s.scheduleMod, s.host
	if err := b.group.Add(&lifecycleStage{name: "schedule_resume", start: func(ctx context.Context) error {
		resumed, err := scheduleMod.Resume(ctx)
		if err != nil {
			return err
		}
		if resumed > 0 {
			logger.InfoContext(ctx, "resumed schedule runs", "component", "schedule", "count", resumed)
		}
		return nil
	}}); err != nil {
		return err
	}
	if err := b.group.Add(server.Background("schedule_pass", scheduleMod.RunPass)); err != nil {
		return err
	}
	recovery := runtimeengine.NewRecovery(b.engine, logger, runtimeengine.DefaultRecoveryInterval)
	if err := b.group.Add(&lifecycleStage{name: "turn_recovery", start: recovery.Start}); err != nil {
		return err
	}
	return b.group.Add(&lifecycleStage{name: "channels", start: host.Start, stop: host.Shutdown})
}

func (b *build) buildOperations() error {
	recorder, err := telemetry.NewOperationRecorder(b.meters())
	if err != nil {
		return err
	}
	registry, err := operation.NewRegistry(operationCatalogs(), b.bindings, &operation.Deps{
		DB:        b.db,
		Effects:   b.broker.Effects(),
		Tools:     b.broker,
		Clock:     b.clock,
		Logger:    b.logger,
		Telemetry: recorder,
	})
	if err != nil {
		return err
	}
	if err := b.broker.RegisterProvider(registry); err != nil {
		return err
	}
	discovery, err := operation.NewDiscovery(&operation.DiscoveryDeps{
		Registry:     registry,
		Behaviors:    b.behaviors,
		Capabilities: b.result.CapabilityReport.Active(),
		Withholdable: b.executor.WithholdableTools(),
		Telemetry:    recorder,
	})
	if err != nil {
		return err
	}
	if err := b.broker.RegisterProvider(discovery); err != nil {
		return err
	}
	if b.builtin == nil {
		return invalidOptions("the operation tools need the builtin tool executor")
	}
	return b.operationPort.Bind(discovery, b.builtin)
}

func (b *build) buildWorkflowHost(ctx context.Context) error {
	if b.workflowMod == nil {
		if err := b.buildWorkflows(ctx, true); err != nil {
			return err
		}
	}
	s := b.server
	s.hostedHandlers = append(s.hostedHandlers, workflow.HandlerName)
	return nil
}

func (b *build) buildDurableHost() error {
	s := b.server
	host, ok := s.broadcastRuntime.(durablesqlite.RunHost)
	if !ok {
		return invalidOptions("the durable runtime cannot host runs")
	}
	stage := durablesqlite.NewHostStage(host, s.hostedHandlers, b.logger)
	if err := b.group.Add(stage); err != nil {
		return err
	}
	if err := b.group.Add(&lifecycleStage{name: "turn_engine", stop: b.engine.Shutdown}); err != nil {
		return err
	}
	recovery := runtimeengine.NewRecovery(b.engine, b.logger, runtimeengine.DefaultRecoveryInterval)
	return b.group.Add(server.Background("turn_recovery_pass", recovery.Run))
}

func (b *build) buildProbe() error {
	cfg, s := b.cfg, b.server
	httpChat, httpChatChecks, err := buildHTTPChat(cfg, b.db, b.engine, b.registry, s.accessRecorder, s.ownerRoute, b.logger)
	if err != nil {
		return err
	}
	s.httpChat = httpChat
	s.checks = append(s.checks, httpChatChecks...)
	probeServer, err := buildProbeServer(cfg, mapCapabilityStatuses(b.result.CapabilityReport), b.events, b.sessions, s.checks...)
	if err != nil {
		return err
	}
	s.listeners = server.New(server.Options{
		Logger:          b.logger,
		ShutdownTimeout: time.Duration(cfg.Runtime.ShutdownTimeout),
	})
	return s.listeners.Add(probeServer)
}

func (b *build) addEdgeListeners() error {
	cfg, s := b.cfg, b.server
	if cfg.Webhook.Enabled {
		runtime, err := b.durableRuntime()
		if err != nil {
			return err
		}
		executions, err := platformwebhook.NewSQLiteStore(b.db)
		if err != nil {
			return err
		}
		turns := turnstore.NewTurnStore(b.db)
		runs, err := workflow.NewStore(b.db, turns)
		if err != nil {
			return err
		}
		githubAdapter, err := github.NewAdapter(newWorkflowCorrelationStore(runs), runtime, b.logger)
		if err != nil {
			return err
		}
		ch, err := gatewaywebhook.New(&gatewaywebhook.Deps{
			Config:     &cfg.Webhook,
			Sessions:   turnstore.NewSessionService(b.db),
			Executions: executions,
			Events:     turns,
			Runtime:    b.engine,
			Keys:       gatewaywebhook.EnvKeySource,
			Handlers:   []gatewaywebhook.EventHandler{githubAdapter},
			Logger:     b.logger,
		})
		if err != nil {
			return err
		}
		if err := s.listeners.Add(ch.Listener()); err != nil {
			return err
		}
		if err := b.group.Add(server.Background("webhook_reconcile", ch.RunReconcile)); err != nil {
			return err
		}
	}
	if cfg.Channels.Telegram.Enabled && cfg.Channels.Telegram.UsesWebhook() {
		telegramWebhook, err := buildTelegramWebhookServer(cfg, s.channels)
		if err != nil {
			return err
		}
		if err := s.listeners.Add(telegramWebhook); err != nil {
			return err
		}
	}
	if s.httpChat != nil {
		if err := s.listeners.Add(s.httpChat); err != nil {
			return err
		}
	}
	b.serve = serveListeners(s.listeners, b.logger, cfg)
	return nil
}

func (b *build) wireWorkflowParts() error {
	s := b.server
	if b.workflowMod == nil || b.interpreter == nil || b.builtin == nil || b.builtin.Broker() == nil {
		return invalidOptions("the server's workflow parts need an interpreter and the surface tool executor")
	}
	stepExecutor, err := b.newAgentStepExecutor()
	if err != nil {
		return err
	}
	if err := b.workflowMod.BindAgentTurns(stepExecutor, b.events); err != nil {
		return fault.Wrap(ErrorCodeInvalidArgument, "the server's workflow module cannot run agent steps", err)
	}
	if err := b.group.Add(server.Background("workflow_proposal_cards", b.workflowMod.ProposalCardsRunner())); err != nil {
		return err
	}
	s.scheduledWorkflows.set(b.interpreter)
	return nil
}

func (b *build) bindErasureRuns() error {
	if b.interpreter != nil {
		b.server.erasureRuns.set(b.interpreter)
	}
	return nil
}

func (b *build) registerSweeps() error {
	cfg, db, logger, artifactRoot := b.cfg, b.db, b.logger, b.artifactRoot
	if err := b.group.Add(server.Background("retention_sweeps", func(ctx context.Context) {
		runRetentionSweeps(ctx, cfg, db, artifactRoot, logger)
	})); err != nil {
		return err
	}
	if err := b.registerRunRetention(); err != nil {
		return err
	}
	if b.workflowMod != nil {
		if run := b.workflowMod.CompletionRunner(); run != nil {
			if err := b.group.Add(server.Background("workflow_completion", run)); err != nil {
				return err
			}
		}
	}
	if b.personaEvalMod == nil {
		return invalidOptions("the server's persona evaluation module is not initialized")
	}
	return b.group.Add(server.Background("persona_eval_schedules", b.personaEvalMod.RunSchedules))
}

func (b *build) registerRunRetention() error {
	prune, ok := b.server.broadcastRuntime.(durablesqlite.Pruner)
	if !ok {
		return invalidOptions("the durable runtime cannot prune ended runs")
	}
	runs, ok := b.server.broadcastRuntime.(runtimeengine.TurnRunStore)
	if !ok {
		return invalidOptions("the durable runtime cannot list and delete turn runs")
	}
	db := b.db
	turns := b.turns
	sweeper, err := durablesqlite.NewRetentionSweeper(&durablesqlite.RetentionSweeperOptions{
		Pruner:   prune,
		Handlers: []string{broadcast.HandlerName, schedule.HandlerName, child.HandlerName, workflow.HandlerName, runtimeengine.HandlerName},
		Orphans: func(ctx context.Context) (int, error) {
			return runtimeengine.DeleteOrphanedTurnRuns(ctx, db, runs, turns)
		},
		Retention: time.Duration(b.cfg.Runtime.RunRetention),
		Batch:     durablesqlite.DefaultRetentionBatch,
		Interval:  durablesqlite.DefaultRetentionInterval,
		Logger:    b.logger,
	})
	if err != nil {
		return err
	}
	return b.group.Add(server.Background("run_retention", sweeper.Run))
}

func serveListeners(listeners *server.Server, logger *slog.Logger, cfg *appconfig.Config) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		logger.InfoContext(ctx, "starting server",
			"component", "server",
			"host", cfg.Server.Host,
			"port", cfg.Server.Port,
		)
		return listeners.Run(ctx)
	}
}

type serverStartup struct {
	config  appconfig.LoadResult
	host    sandbox.HostPreparation
	hostErr error
}

func prepareHostThenLoad(path string, prepare func() (sandbox.HostPreparation, error), load func(string) (appconfig.LoadResult, error)) (serverStartup, error) {
	host, hostErr := prepare()
	result, err := load(path)
	if err != nil {
		return serverStartup{}, err
	}
	return serverStartup{config: result, host: host, hostErr: hostErr}, nil
}

func logSandboxHost(ctx context.Context, logger *slog.Logger, prepared sandbox.HostPreparation, err error) {
	if err != nil {
		logger.WarnContext(ctx, "sandbox host not prepared", "component", "sandbox", "error", err)
		return
	}
	logger.InfoContext(ctx, "sandbox host prepared", "component", "sandbox", "cgroup_root", prepared.CgroupRoot, "mode", prepared.Mode)
}

type sessionAgentGetter struct {
	sessions turnstore.SessionService
}

func (g sessionAgentGetter) GetSessionAgent(ctx context.Context, sessionID string) (agentID string, found bool, err error) {
	sess, err := g.sessions.Get(ctx, sessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	return sess.AgentID, true, nil
}

func (b *build) chatToolWiring(ctx context.Context) (toolWiring, error) {
	chat, opts := b.chat, &b.opts.Chat
	inFile, inOk := opts.In.(*os.File)
	outFile, outOk := opts.Out.(*os.File)
	if inOk && outOk {
		chat.inFile, chat.outFile = inFile, outFile
		chat.useTTY = shouldUseTTY(chatPresentation{plain: opts.Plain, noColor: opts.NoColor}, terminal.IsTerminal(int(inFile.Fd())), terminal.IsTerminal(int(outFile.Fd())))
	}
	agentID, err := auraagent.ResolveChatPersona(ctx, b.registry, sessionAgentGetter{sessions: b.sessions}, opts.SessionID, opts.Persona)
	if err != nil {
		return toolWiring{}, err
	}
	chat.agentID = agentID
	if b.cfg.Tools != nil && chat.useTTY {
		chat.approvals = terminal.NewApprovalBridge()
	}
	return toolWiring{
		decider:   terminal.ApprovalDecider(chat.approvals),
		reachable: terminal.ApprovalReachable(chat.approvals),
	}, nil
}

func (b *build) buildChatSurface() error {
	cfg, chat, opts := b.cfg, b.chat, &b.opts.Chat
	runner, err := terminal.NewRuntimeRunner(b.engine, chat.agentID)
	if err != nil {
		return err
	}
	sessions, err := terminal.NewTurnSessions(b.sessions, chat.agentID)
	if err != nil {
		return err
	}
	commandRecorder, err := telemetry.NewCommandRecorder(nil, nil)
	if err != nil {
		return err
	}
	var skillList command.Skills
	if b.modules.skills != nil {
		skillList = b.modules.skills
	}
	var closeInput func()
	if closer, ok := opts.In.(io.Closer); ok {
		closeInput = func() { _ = closer.Close() }
	}
	var tty *terminal.TTYRenderer
	var approvals *terminal.ApprovalBridge
	if chat.useTTY {
		outFile := chat.outFile
		outFD := int(outFile.Fd())
		tty = terminal.NewTTYRenderer(terminal.TTYOptions{
			Out:     terminal.NewTTYOutput(outFile),
			Width:   func() int { w, _ := terminal.TerminalSize(outFD); return w },
			Hz:      cfg.Terminal.RenderHz,
			Styling: !opts.NoColor,
			Stdin:   chat.inFile,
			Stdout:  outFile,
		})
		approvals = chat.approvals
	}
	console, err := terminal.New(&terminal.Deps{
		Runner:          runner,
		Sessions:        sessions,
		Renderer:        terminal.PlainRenderer{},
		In:              opts.In,
		Out:             opts.Out,
		Diag:            opts.Diag,
		Config:          cfg.Terminal,
		Principal:       principal.OwnerID,
		SessionID:       opts.SessionID,
		AgentID:         chat.agentID,
		Skills:          skillList,
		CommandRecorder: commandRecorder,
		CloseInput:      closeInput,
		TTY:             tty,
		Approvals:       approvals,
	})
	if err != nil {
		return err
	}
	b.serve = serveConsole(console, opts.Interrupts)
	return nil
}

func serveConsole(console *terminal.Console, signals <-chan os.Signal) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		interrupts, stopInterrupts := terminal.ForwardInterrupts(ctx, signals)
		defer stopInterrupts()
		return console.RunWithInterrupts(ctx, interrupts)
	}
}

type interruptContextKey struct{}

func WithInterrupts(ctx context.Context, signals <-chan os.Signal) context.Context {
	return context.WithValue(ctx, interruptContextKey{}, signals)
}

func interruptsFromContext(ctx context.Context) <-chan os.Signal {
	signals, _ := ctx.Value(interruptContextKey{}).(<-chan os.Signal)
	return signals
}

func defaultProcessProbe() (health.ProcessStatus, bool) {
	return health.ProcessProbe(sandbox.AccountingCgroup)
}

type healthEventStoreAdapter struct {
	events   turnstore.EventStore
	sessions turnstore.SessionService
}

func (a healthEventStoreAdapter) LastSequence(ctx context.Context, sessionID string) (uint64, error) {
	return a.events.LastSequence(ctx, sessionID)
}

func (a healthEventStoreAdapter) Append(ctx context.Context, record *health.EventRecord) error {
	return a.events.Append(ctx, &turn.Event{
		ID:            record.ID,
		SessionID:     record.SessionID,
		Sequence:      record.Sequence,
		Author:        record.Author,
		Kind:          record.Kind,
		SchemaVersion: record.SchemaVersion,
		Payload:       record.Payload,
		CreatedAt:     record.CreatedAt,
	})
}

func (a healthEventStoreAdapter) EnsureSystemSession(ctx context.Context, sessionID, ownerID string) error {
	_, err := a.sessions.Get(ctx, sessionID)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := a.sessions.Create(ctx, &turn.Session{ID: sessionID, OwnerID: ownerID}); err != nil {
		if code, ok := fault.CodeOf[turnstore.ErrorCode](err); ok && code == turnstore.ErrorCodeSessionIDConflict {
			return nil
		}
		return err
	}
	return nil
}

func (a healthEventStoreAdapter) ListSessionEvents(ctx context.Context, sessionID string, afterSequence uint64, limit int) ([]health.StoredEvent, bool, error) {
	listed, err := a.sessions.ListEvents(ctx, sessionID, afterSequence, limit)
	if err != nil {
		if code, ok := fault.CodeOf[turnstore.ErrorCode](err); ok && code == turnstore.ErrorCodeSessionNotFound {
			return nil, false, nil
		}
		return nil, false, err
	}
	out := make([]health.StoredEvent, 0, len(listed))
	for i := range listed {
		out = append(out, health.StoredEvent{
			ID:       listed[i].ID,
			Sequence: listed[i].Sequence,
			Kind:     listed[i].Kind,
			Payload:  listed[i].Payload,
		})
	}
	return out, true, nil
}

func buildProbeServer(cfg *appconfig.Config, capabilities []health.CapabilityStatus, events turnstore.EventStore, sessions turnstore.SessionService, extra ...health.RegisteredCheck) (*health.ProbeServer, error) {
	if cfg == nil {
		return nil, errors.New("probe listener requires configuration")
	}
	registry, err := buildHealthRegistry(cfg, capabilities, sandboxSupport(sandbox.Negotiate), defaultProcessProbe, extra...)
	if err != nil {
		return nil, err
	}
	var tracker *health.StateTracker
	if events != nil && sessions != nil {
		adapter := healthEventStoreAdapter{events: events, sessions: sessions}
		log := health.NewEventLog(adapter, adapter)
		policy := health.TransitionPolicy{
			StableFor: time.Duration(cfg.Health.CheckInterval),
			Cooldown:  max(2*time.Duration(cfg.Health.CheckInterval), time.Minute),
		}
		tracker, err = health.NewStateTracker(policy, log.Sink, log.History)
		if err != nil {
			return nil, err
		}
	}
	return health.NewProbeServer(&health.ProbeServerOptions{
		Listen:        cfg.Health.Listen,
		Version:       version,
		Registry:      registry,
		CheckTimeout:  time.Duration(cfg.Health.CheckTimeout),
		CheckInterval: time.Duration(cfg.Health.CheckInterval),
		Tracker:       tracker,
	})
}

func runStatus(cmd *cobra.Command, cfg *appconfig.Config, report capability.Report, negotiate func() (sandbox.Primitives, error), offline, asJSON bool, processProbe func() (health.ProcessStatus, bool)) error {
	ctx := cmd.Context()
	live, liveReachable := health.LiveReadiness(ctx, offline, cfg.Health.Listen)
	daemon := health.DaemonSandboxState(live, liveReachable)
	interpreterChecks, err := interpreterStatusChecks(cfg, report)
	if err != nil {
		return clikit.NewExitCodeError(health.ExitCommand, err)
	}
	knowledgeChecks, err := knowledgeStatusChecks(cfg)
	if err != nil {
		return clikit.NewExitCodeError(health.ExitCommand, err)
	}
	interpreterChecks = append(interpreterChecks, knowledgeChecks...)
	registry, err := buildHealthRegistry(cfg, mapCapabilityStatuses(report), statusSandboxSupport(daemon, negotiate), processProbe, interpreterChecks...)
	if err != nil {
		return clikit.NewExitCodeError(health.ExitCommand, err)
	}
	findings := registry.Evaluate(ctx)
	primitives, negotiateErr := negotiate()
	sandboxText, sandboxAvailable := statusSandboxLine(primitives, negotiateErr, daemon)
	if asJSON {
		return health.WriteStatusJSON(cmd.OutOrStdout(), sandboxAvailable, findings, live, liveReachable)
	}
	return health.WriteStatusText(cmd.OutOrStdout(), sandboxText, sandboxAvailable, findings, live, liveReachable)
}

func buildHealthRegistry(cfg *appconfig.Config, capabilities []health.CapabilityStatus, sandboxCheck func() (supported bool, detail string), processProbe func() (health.ProcessStatus, bool), extra ...health.RegisteredCheck) (*health.Registry, error) {
	dbPath, artifactRoot, backupDir, err := storage.ResolvePaths(&cfg.Storage)
	if err != nil {
		return nil, err
	}
	checkTimeout := time.Duration(cfg.Health.CheckTimeout)
	checks := make([]health.RegisteredCheck, 0, 9+len(extra))
	checks = append(checks,
		health.RegisteredCheck{
			ID:          "migration",
			Checker:     health.MigrationChecker{Versions: func(ctx context.Context) (applied, latest int, err error) { return schemaVersionsReadOnly(ctx, dbPath) }},
			Timeout:     checkTimeout,
			Remediation: health.RemediationRunMigrations,
		},
		health.RegisteredCheck{
			ID:          "backup",
			Checker:     health.BackupChecker{LastBackup: func(context.Context) (time.Time, error) { return health.LastBackupTime(backupDir) }, MaxAge: time.Duration(cfg.Health.BackupMaxAge)},
			Timeout:     checkTimeout,
			Remediation: health.RemediationRestoreBackup,
		},
		health.RegisteredCheck{
			ID:          "storage",
			Checker:     health.StorageChecker{Intake: health.StorageIntakeProbe(dbPath, storage.WritableProbe, storage.ClassifyOpenError, storage.DiskFreeBytes)},
			Timeout:     checkTimeout,
			Remediation: health.RemediationRepairStorage,
		},
		health.RegisteredCheck{
			ID: "disk",
			Checker: health.DiskChecker{
				Targets: func(context.Context) []health.FilesystemTarget {
					return health.DiskTargets(dbPath, artifactRoot, backupDir)
				},
				Usage: func(_ context.Context, path string) (health.DiskUsage, error) {
					free, total, inodes, err := storage.DiskUsage(path)
					return health.DiskUsage{FreeBytes: free, TotalBytes: total, FreeInodes: inodes}, err
				},
				WarningPercent:  cfg.Health.DiskWarningPercent,
				CriticalPercent: cfg.Health.DiskCriticalPercent,
				FloorBytes:      int64(cfg.Health.DiskCriticalFloorBytes),
			},
			Timeout:     checkTimeout,
			Remediation: health.RemediationRepairStorage,
		},
		health.RegisteredCheck{
			ID:          "capability",
			Checker:     health.CapabilityChecker{Statuses: func(context.Context) []health.CapabilityStatus { return capabilities }},
			Timeout:     checkTimeout,
			Remediation: health.RemediationReviewProfile,
		},
		health.RegisteredCheck{
			ID:          "process",
			Checker:     health.ProcessChecker{Probe: func(context.Context) (health.ProcessStatus, bool) { return processProbe() }},
			Timeout:     checkTimeout,
			Remediation: health.RemediationRelieveLimits,
		},
		health.RegisteredCheck{
			ID:          "sandbox",
			Checker:     health.SandboxChecker{Support: sandboxCheck},
			Timeout:     checkTimeout,
			Remediation: health.RemediationReviewSandbox,
		},
		health.RegisteredCheck{
			ID:          "provider",
			Checker:     health.ProviderChecker{Probe: func(context.Context) error { return providerConfigured(cfg) }},
			Timeout:     checkTimeout,
			Remediation: health.RemediationConfigureModels,
		},
	)
	return health.NewRegistry(append(checks, extra...)...)
}

func mapCapabilityStatuses(report capability.Report) []health.CapabilityStatus {
	reported := report.Statuses()
	mapped := make([]health.CapabilityStatus, 0, len(reported))
	for _, s := range reported {
		mapped = append(mapped, health.CapabilityStatus{
			Name:              s.Name,
			Compiled:          s.Compiled,
			Available:         s.Available,
			Enabled:           s.Enabled,
			MissingDependency: s.MissingDependency,
			UnavailableReason: s.UnavailableReason,
		})
	}
	return mapped
}

func schemaVersionsReadOnly(ctx context.Context, dbPath string) (applied, latest int, err error) {
	if _, err := os.Stat(dbPath); err != nil {
		return 0, 0, err
	}
	if err := sqlite.CheckCompatibility(ctx, dbPath); err != nil {
		return 0, 0, err
	}
	db, err := sqlite.OpenReadOnly(ctx, dbPath)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = db.Close() }()
	return sqlite.SchemaVersions(ctx, db, Migrations())
}

func sandboxSupport(negotiate func() (sandbox.Primitives, error)) func() (bool, string) {
	return func() (bool, string) {
		primitives, err := negotiate()
		if err != nil {
			return false, err.Error()
		}
		if missing := sandbox.MissingMandatory(primitives); len(missing) > 0 {
			return false, withCgroupDetail("missing: "+strings.Join(missing, ", "), primitives.CgroupDetail)
		}
		return true, withCgroupDetail("all containment primitives available", primitives.CgroupDetail)
	}
}

func withCgroupDetail(detail, cgroupDetail string) string {
	if cgroupDetail == "" {
		return detail
	}
	return detail + " (" + cgroupDetail + ")"
}

func localSandboxSupport(negotiate func() (sandbox.Primitives, error)) func() (bool, string) {
	support := sandboxSupport(negotiate)
	return func() (bool, string) {
		supported, detail := support()
		return supported, "scope=cli: " + detail
	}
}

func statusSandboxSupport(daemon health.DaemonSandbox, negotiate func() (sandbox.Primitives, error)) func() (bool, string) {
	local := localSandboxSupport(negotiate)
	return func() (bool, string) {
		switch daemon {
		case health.DaemonSandboxUp:
			return true, "verified by running daemon"
		case health.DaemonSandboxDown:
			return false, "reported by running daemon"
		default:
			return local()
		}
	}
}

func statusSandboxLine(have sandbox.Primitives, negotiateErr error, daemon health.DaemonSandbox) (string, bool) {
	if daemon == health.DaemonSandboxUp {
		return "sandbox: available", true
	}
	return formatSandboxStatus(have, negotiateErr)
}

func formatSandboxStatus(have sandbox.Primitives, negotiateErr error) (string, bool) {
	if negotiateErr != nil {
		return "sandbox: unavailable\nreason: " + negotiateErr.Error(), false
	}
	missing := sandbox.MissingMandatory(have)
	if len(missing) == 0 {
		return "sandbox: available", true
	}
	return "sandbox: unavailable\nmissing: " + strings.Join(missing, ", "), false
}

func providerConfigured(cfg *appconfig.Config) error {
	definition, ok := cfg.Models.Definitions["primary"]
	if !ok || definition.Model == "" {
		return errors.New("no primary model definition")
	}
	return nil
}
