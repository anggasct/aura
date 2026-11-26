package app

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/channels/email"
	"github.com/anggasct/aura/internal/channels/httpchat"
	"github.com/anggasct/aura/internal/channels/kit/approval"
	gatewaywebhook "github.com/anggasct/aura/internal/channels/webhook"
	"github.com/anggasct/aura/internal/modules/broadcast"
	"github.com/anggasct/aura/internal/modules/child"
	"github.com/anggasct/aura/internal/modules/github"
	"github.com/anggasct/aura/internal/modules/handoff"
	"github.com/anggasct/aura/internal/modules/mcp"
	"github.com/anggasct/aura/internal/modules/plugin"
	"github.com/anggasct/aura/internal/modules/schedule"
	"github.com/anggasct/aura/internal/modules/workflow"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/durable/sqlite"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/sandbox"
	"github.com/anggasct/aura/internal/platform/server"
	"github.com/anggasct/aura/internal/platform/telemetry"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
	runtimeengine "github.com/anggasct/aura/internal/platform/turn/engine"
	"github.com/anggasct/aura/internal/platform/turn/store"
	platformwebhook "github.com/anggasct/aura/internal/platform/webhook"
)

func newServerCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "server",
		Short: "Start the daemon with all listeners",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			startup, err := prepareHostThenLoad(gf.configPath, sandbox.PrepareHost, appconfig.Load)
			if err != nil {
				return err
			}
			level, format := resolveLogging(cmd, startup.config.Config)
			app, err := Build(ctx, &Options{
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
			return runAndClose(ctx, app)
		},
	}
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
	if err := recoverEmailEffects(ctx, cfg, db, b.logger); err != nil {
		return toolWiring{}, err
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
	return toolWiring{
		observer:  toolObserver(toolRecorder),
		decider:   approval.Decider(approvalTargets),
		access:    newAccessPolicy(b.registry, db, built.accessRecorder),
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
	sessions, err := sqlite.NewSessionStore(&sqlite.SessionStoreOptions{DB: b.db, Turns: b.turns})
	if err != nil {
		return err
	}
	b.server.durableConfig = &runtimeengine.DurableConfig{Sessions: sessions, Runtime: runtime}
	b.server.hostedHandlers = append(b.server.hostedHandlers, runtimeengine.HandlerName)
	return nil
}

func (b *build) buildServerSurface(ctx context.Context) error {
	steps := []func() error{
		b.buildChannelHost,
		b.buildBroadcast,
		b.buildSchedule,
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
	})
	if err != nil {
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
	recovery := &turnRecovery{recoverer: b.engine, logger: logger, interval: turnRecoveryInterval}
	if err := b.group.Add(&lifecycleStage{name: "turn_recovery", start: recovery.start}); err != nil {
		return err
	}
	return b.group.Add(&lifecycleStage{name: "channels", start: host.Start, stop: host.Shutdown})
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
	host, ok := s.broadcastRuntime.(durableRunHost)
	if !ok {
		return invalidOptions("the durable runtime cannot host runs")
	}
	stage := &durableHostStage{host: host, handlers: s.hostedHandlers, logger: b.logger}
	if err := b.group.Add(&lifecycleStage{name: "durable_host", start: stage.start, stop: stage.stop}); err != nil {
		return err
	}
	if err := b.group.Add(&lifecycleStage{name: "turn_engine", stop: b.engine.Shutdown}); err != nil {
		return err
	}
	recovery := &turnRecovery{recoverer: b.engine, logger: b.logger, interval: turnRecoveryInterval}
	return b.group.Add(server.Background("turn_recovery_pass", recovery.run))
}

func (b *build) buildProbe() error {
	cfg, s := b.cfg, b.server
	httpChat, httpChatChecks, err := buildHTTPChat(cfg, b.db, b.engine, b.registry, s.accessRecorder, s.ownerRoute, b.logger)
	if err != nil {
		return err
	}
	s.httpChat = httpChat
	s.checks = append(s.checks, httpChatChecks...)
	probeListener, err := b.newProbeListener()
	if err != nil {
		return err
	}
	s.listeners = server.New(server.Options{
		Logger:          b.logger,
		ShutdownTimeout: time.Duration(cfg.Runtime.ShutdownTimeout),
	})
	return s.listeners.Add(probeListener)
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

func (b *build) newProbeListener() (*probeListener, error) {
	return buildProbeListener(b.cfg, mapCapabilityStatuses(b.result.CapabilityReport), b.events, b.sessions, b.server.checks...)
}

func (b *build) wireWorkflowParts() error {
	s := b.server
	if b.workflowMod == nil || b.interpreter == nil || b.builtin == nil || b.builtin.Broker() == nil {
		return invalidOptions("the server's workflow parts need an interpreter and the surface tool executor")
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
	prune, ok := b.server.broadcastRuntime.(pruner)
	if !ok {
		return invalidOptions("the durable runtime cannot prune ended runs")
	}
	runs, ok := b.server.broadcastRuntime.(runtimeengine.TurnRunStore)
	if !ok {
		return invalidOptions("the durable runtime cannot list and delete turn runs")
	}
	db := b.db
	turns := b.turns
	sweep := &runRetention{
		pruner: prune,
		orphans: func(ctx context.Context) (int, error) {
			return runtimeengine.DeleteOrphanedTurnRuns(ctx, db, runs, turns)
		},
		retention: time.Duration(b.cfg.Runtime.RunRetention),
		batch:     runRetentionBatch,
		interval:  runRetentionInterval,
		logger:    b.logger,
	}
	return b.group.Add(server.Background("run_retention", sweep.run))
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
