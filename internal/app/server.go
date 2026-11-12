package app

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/channels/email"
	"github.com/anggasct/aura/internal/channels/httpchat"
	"github.com/anggasct/aura/internal/channels/kit/approval"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/modules/broadcast"
	"github.com/anggasct/aura/internal/modules/builtin"
	"github.com/anggasct/aura/internal/modules/child"
	"github.com/anggasct/aura/internal/modules/schedule"
	"github.com/anggasct/aura/internal/modules/workflow"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/sandbox"
	"github.com/anggasct/aura/internal/platform/server"
	"github.com/anggasct/aura/internal/platform/telemetry"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/turn/channelhost"
	"github.com/anggasct/aura/internal/platform/turn/engine"
	"github.com/anggasct/aura/internal/store"
)

func newServerCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "server",
		Short: "Start the daemon with all listeners",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			startup, err := prepareHostThenLoad(gf.configPath, sandbox.PrepareHost, config.Load)
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
	accessRecorder     *telemetry.AccessRecorder
	mcp                *mcpRuntime
	personaHook        postTurnHook
	durableConfig      *runtimeengine.DurableConfig
	host               *runtimeengine.Host
	broadcastRuntime   durable.Runtime
	hostedHandlers     []string
	broadcastWiring    *broadcastChannels
	broadcastRunner    *broadcast.Runner
	scheduleRunner     *schedule.Runner
	scheduledWorkflows *scheduleWorkflows
	childLedger        child.BudgetLedger
	listeners          *server.Server
	httpChat           *httpchat.Server
}

func durableEnabled(cfg *config.Config) bool {
	return cfg.Durable != nil && cfg.Durable.Enabled
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
		recorder.Record(ctx, &telemetry.ToolObservation{
			Name:          observation.ToolName + "@" + observation.ToolVersion,
			Status:        string(observation.Class),
			PolicyOutcome: observation.PolicyOutcome,
			Approval:      observation.Approval,
			Executor:      observation.Executor,
			Duration:      observation.Duration,
			OutputBytes:   observation.OutputBytes,
		})
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
	s.personaHook = personaTurnHook(personaRecorder)
	personaRouter, err := auraagent.NewRouter(b.registry, &cfg.Channels, cfg.Bindings)
	if err != nil {
		return toolWiring{}, err
	}
	if err := recoverEmailEffects(ctx, cfg, db, b.logger); err != nil {
		return toolWiring{}, err
	}
	channelAdapters, channelChecks, approvalAdapters, err := buildChannelAdapters(cfg, personaRouter, db, b.artifactRoot, meters, b.logger)
	if err != nil {
		return toolWiring{}, err
	}
	accessRecorder, err := telemetry.NewAccessRecorder(meters)
	if err != nil {
		return toolWiring{}, err
	}
	var ownerRoute *approval.OwnerRoute
	if cfg.Channels.HTTP.Enabled || cfg.Channels.Email.Enabled {
		ownerRoute, err = newOwnerRoute(approvalAdapters, accessRecorder.ApprovalRouted)
		if err != nil {
			return toolWiring{}, err
		}
	}
	s.channels, s.checks, s.approvalAdapters = channelAdapters, channelChecks, approvalAdapters
	s.ownerRoute, s.accessRecorder = ownerRoute, accessRecorder
	s.emailMailboxes = wireEmailOwnerAccess(channelAdapters, ownerRoute, approvalAdapters)
	approvalTargets := approvalChannels(approvalAdapters, ownerRoute)
	return toolWiring{
		observer:  toolObserver(toolRecorder),
		decider:   channelApprovalDecider(approvalTargets),
		access:    newAccessPolicy(b.registry, db, accessRecorder),
		reachable: channelApprovalReachable(approvalTargets),
	}, nil
}

func (b *build) buildServerTools(ctx context.Context) error {
	cfg, db, s := b.cfg, b.db, b.server
	notifiers := handoffNotifiers(s.channels)
	if cfg.Channels.HTTP.Enabled {
		notifiers = append(notifiers, newHTTPHandoffNotifier(s.ownerRoute, s.approvalAdapters))
	}
	b.builtin.SetHandoffOpener(newHandoffService(db, notifiers))
	b.builtin.SetEmailOutbox(&emailOutbox{adapters: s.emailMailboxes})
	plugins, err := planPlugins(ctx, cfg, db, b.builtin.Broker())
	if err != nil {
		return err
	}
	plugins.warnUnavailablePlugins(ctx, b.logger, b.registry.Definitions())
	if err := b.startConnectorPlugins(ctx, plugins); err != nil {
		return err
	}
	if err := b.startMCPServers(ctx, plugins); err != nil {
		return err
	}
	pluginCheck, pluginChecked, err := plugins.healthCheck(cfg, db, s.mcp)
	if err != nil {
		return err
	}
	if pluginChecked {
		s.checks = append(s.checks, pluginCheck)
	}
	if b.primaryModel() == "" {
		return &config.Error{Code: config.ErrorCodeConfigInvalid, Detail: "models.definitions.primary.model is required for the server runtime"}
	}
	return nil
}

func (b *build) startConnectorPlugins(ctx context.Context, plugins *pluginRuntime) error {
	connectorPlugins := plugins.connectorPlugins()
	if len(connectorPlugins) == 0 {
		return nil
	}
	logger := b.logger
	connectors, err := startConnectorPlugins(ctx, &connectorRuntimeOptions{
		Broker: b.builtin.Broker(), Logger: logger, Capabilities: b.result.CapabilityReport, Plugins: connectorPlugins,
	})
	if err != nil {
		return err
	}
	plugins.connectors = connectors
	return b.group.Defer("plugin_connector", func(ctx context.Context) error {
		if err := connectors.Close(ctx); err != nil {
			logger.WarnContext(ctx, "connector plugin shutdown", "component", "plugin_connector", "error", err)
		}
		return nil
	})
}

func (b *build) startMCPServers(ctx context.Context, plugins *pluginRuntime) error {
	cfg, logger := b.cfg, b.logger
	if !mcpServersConfigured(cfg) && len(plugins.mcpPlugins()) == 0 {
		return nil
	}
	mcpServers, err := startConfiguredMCP(ctx, cfg, b.builtin.Broker(), logger, b.meters(), b.result.CapabilityReport, b.registry.Definitions(), plugins.mcpPlugins())
	if err != nil {
		return err
	}
	b.server.mcp = mcpServers
	if err := b.group.Defer("mcp", func(ctx context.Context) error {
		if err := mcpServers.Close(ctx); err != nil {
			logger.WarnContext(ctx, "mcp shutdown", "component", "mcp", "error", err)
		}
		return nil
	}); err != nil {
		return err
	}
	if mcpServers.hasCheck {
		b.server.checks = append(b.server.checks, mcpServers.check)
	}
	return nil
}

func (b *build) buildServerDurable() error {
	if !durableEnabled(b.cfg) {
		return nil
	}
	sessionRuntime, err := b.durableRuntime()
	if err != nil {
		return err
	}
	b.server.durableConfig = &runtimeengine.DurableConfig{
		Sessions: &durableSessionStore{runtime: sessionRuntime},
		Runtime:  sessionRuntime,
	}
	return nil
}

func (b *build) buildServerSurface(ctx context.Context) error {
	steps := []func() error{
		b.buildChannelHost,
		b.buildBroadcast,
		b.buildSchedule,
		b.buildDurableHost,
		b.buildProbe,
		func() error { return b.wireDurableListener(ctx) },
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
	s, meters := b.server, b.meters()
	commandRecorder, err := telemetry.NewCommandRecorder(b.pipeline.TracerProvider(), meters)
	if err != nil {
		return err
	}
	if err := wireChatCommands(s.channels, b.engine, b.sessions, b.events, store.NewConversationSessionStore(b.db), b.modules.skills, commandRecorder); err != nil {
		return err
	}
	s.host, err = runtimeengine.NewHost(b.engine, s.channels, b.logger)
	return err
}

func (b *build) buildBroadcast() error {
	cfg, db, logger, s := b.cfg, b.db, b.logger, b.server
	broadcastEffects, err := toolsbuiltin.NewChannelEffects(db, logger)
	if err != nil {
		return err
	}
	s.broadcastWiring = buildBroadcastSenders(s.channels, broadcastEffects, b.sessions)
	broadcastRecorder, err := telemetry.NewBroadcastRecorder(b.meters())
	if err != nil {
		return err
	}
	s.broadcastRunner, err = buildBroadcastRunner(cfg, db, logger, s.broadcastWiring, broadcastRecorderObserver(broadcastRecorder))
	if err != nil {
		return err
	}
	s.broadcastRuntime, err = b.durableRuntime()
	if err != nil {
		return err
	}
	if !durableEnabled(cfg) {
		if err := registerBroadcastHandler(s.broadcastRuntime, s.broadcastRunner); err != nil {
			return err
		}
		s.hostedHandlers = append(s.hostedHandlers, broadcast.HandlerName)
	}
	runtime := s.broadcastRuntime
	return b.group.Add(&lifecycleStage{name: "broadcast_resume", start: func(ctx context.Context) error {
		resumed, err := resumeBroadcastRunners(ctx, runtime, db)
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
	cronBroadcaster, err := buildCronBroadcaster(cfg, db, s.broadcastRuntime, s.broadcastWiring)
	if err != nil {
		return err
	}
	scheduleRecorder, err := telemetry.NewScheduleRecorder(b.meters())
	if err != nil {
		return err
	}
	s.scheduledWorkflows = &scheduleWorkflows{}
	s.scheduleRunner, err = buildScheduleRunner(&scheduleRunnerInput{
		db: db, logger: logger, engine: b.engine, broadcaster: cronBroadcaster,
		retention: time.Duration(cfg.Scheduler.OccurrenceRetention), observer: scheduleRecorderObserver(scheduleRecorder),
		workflows: s.scheduledWorkflows, starter: s.broadcastRuntime,
	})
	if err != nil {
		return err
	}
	if !durableEnabled(cfg) {
		if err := registerScheduleHandler(s.broadcastRuntime, s.scheduleRunner); err != nil {
			return err
		}
		s.hostedHandlers = append(s.hostedHandlers, schedule.HandlerName)
	}
	s.childLedger = child.NewLedger(nil, 0)
	if cfg.Children != nil && cfg.Children.Enabled {
		childHandler, err := buildChildHandler(db, s.childLedger, &runtimeSignaler{runtime: s.broadcastRuntime}, b.executor, b.primaryModel())
		if err != nil {
			return err
		}
		if !durableEnabled(cfg) {
			if err := registerChildHandler(s.broadcastRuntime, childHandler); err != nil {
				return err
			}
			s.hostedHandlers = append(s.hostedHandlers, child.HandlerName)
		}
	}
	runtime, host := s.broadcastRuntime, s.host
	if err := b.group.Add(&lifecycleStage{name: "schedule_resume", start: func(ctx context.Context) error {
		resumed, err := resumeScheduleRuns(ctx, runtime, db)
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
	pass := &schedulePass{runtime: runtime, db: db, logger: logger, interval: schedulePassInterval}
	if err := b.group.Add(server.Background("schedule_pass", pass.run)); err != nil {
		return err
	}
	return b.group.Add(&lifecycleStage{name: "channels", start: host.Start, stop: host.Shutdown})
}

func (b *build) buildDurableHost() error {
	if !durableEnabled(b.cfg) {
		s := b.server
		host, ok := s.broadcastRuntime.(durableRunHost)
		if !ok {
			return invalidOptions("the durable runtime cannot host runs")
		}
		stage := &durableHostStage{host: host, handlers: s.hostedHandlers, logger: b.logger}
		if err := b.group.Add(&lifecycleStage{name: "durable_host", start: stage.start, stop: stage.stop}); err != nil {
			return err
		}
	}
	return b.group.Add(&lifecycleStage{name: "turn_engine", stop: b.engine.Shutdown})
}

func (b *build) buildProbe() error {
	cfg, s := b.cfg, b.server
	s.checks = append(s.checks, personaEvalCheck(cfg, b.registry, b.db))
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
		webhookListener, err := buildWebhookListener(&webhookListenerInput{cfg: cfg, db: b.db, backend: b.engine, logger: b.logger, runtime: runtime})
		if err != nil {
			return err
		}
		if err := s.listeners.Add(webhookListener); err != nil {
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

func (b *build) wireDurableListener(ctx context.Context) error {
	cfg, db, s := b.cfg, b.db, b.server
	if !durableEnabled(cfg) {
		return nil
	}
	if err := b.buildWorkflows(ctx); err != nil {
		return err
	}
	durableListener, err := buildDurableListener(&durableListenerInput{
		result: b.result, db: db, logger: b.logger, engine: b.engine, interpreter: b.interpreter,
	})
	if err != nil {
		return err
	}
	workflowTools, err := newWorkflowToolController(durableListener.interpreter, workflow.NewStore(db), b.registry, replyTargeters(s.channels)...)
	if err != nil {
		return err
	}
	workflowTools.enableProposalCards(cfg, proposalOfferers(s.channels), b.logger)
	if err := b.group.Add(server.Background("workflow_proposal_cards", workflowTools.runProposalCards)); err != nil {
		return err
	}
	b.builtin.SetWorkflowController(workflowTools)
	s.scheduledWorkflows.set(durableListener.interpreter)
	if err := registerBroadcastHandler(durableListener, s.broadcastRunner); err != nil {
		return err
	}
	if err := registerScheduleHandler(durableListener, s.scheduleRunner); err != nil {
		return err
	}
	if cfg.Children != nil && cfg.Children.Enabled {
		childHandler, err := buildChildHandler(db, s.childLedger, nil, b.executor, b.primaryModel())
		if err != nil {
			return err
		}
		if err := registerChildHandler(durableListener, childHandler); err != nil {
			return err
		}
	}
	return s.listeners.Add(durableListener)
}

func (b *build) registerSweeps() error {
	cfg, db, registry, logger, artifactRoot := b.cfg, b.db, b.registry, b.logger, b.artifactRoot
	if err := b.group.Add(server.Background("retention_sweeps", func(ctx context.Context) {
		runRetentionSweeps(ctx, cfg, db, artifactRoot, logger)
	})); err != nil {
		return err
	}
	if err := b.registerRunRetention(); err != nil {
		return err
	}
	if durableEnabled(cfg) {
		if err := b.group.Add(server.Background("workflow_completion", func(ctx context.Context) {
			newWorkflowCompletionNotifier(cfg, db, registry, logger).run(ctx, workflowCompletionInterval)
		})); err != nil {
			return err
		}
	}
	configPath := b.result.Path
	models := behaviorModels{defaultModel: b.primaryModel(), route: b.routeModel, models: b.models}
	return b.group.Add(server.Background("persona_eval_schedules", func(ctx context.Context) {
		runPersonaEvalSchedules(ctx, configPath, db, registry, models, logger, wallEvalClock)
	}))
}

func (b *build) registerRunRetention() error {
	if durableEnabled(b.cfg) {
		return nil
	}
	prune, ok := b.server.broadcastRuntime.(pruner)
	if !ok {
		return invalidOptions("the durable runtime cannot prune ended runs")
	}
	sweep := &runRetention{
		pruner:    prune,
		retention: time.Duration(b.cfg.Runtime.RunRetention),
		batch:     runRetentionBatch,
		interval:  runRetentionInterval,
		logger:    b.logger,
	}
	return b.group.Add(server.Background("run_retention", sweep.run))
}

func serveListeners(listeners *server.Server, logger *slog.Logger, cfg *config.Config) func(ctx context.Context) error {
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
	config  config.LoadResult
	host    sandbox.HostPreparation
	hostErr error
}

func prepareHostThenLoad(path string, prepare func() (sandbox.HostPreparation, error), load func(string) (config.LoadResult, error)) (serverStartup, error) {
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
