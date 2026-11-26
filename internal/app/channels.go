package app

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/metric"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/channels/discord"
	"github.com/anggasct/aura/internal/channels/email"
	"github.com/anggasct/aura/internal/channels/kit/access"
	"github.com/anggasct/aura/internal/channels/kit/approval"
	"github.com/anggasct/aura/internal/channels/kit/media"
	"github.com/anggasct/aura/internal/channels/telegram"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/storage"
	"github.com/anggasct/aura/internal/platform/telemetry"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

type approvalAdapter interface {
	approval.Channel
	approval.PersonaOwnerApprover
	ownerHandoffNotifier
}

func ownerApprovers(adapters []approvalAdapter) []approval.PersonaOwnerApprover {
	approvers := make([]approval.PersonaOwnerApprover, 0, len(adapters))
	for _, adapter := range adapters {
		approvers = append(approvers, adapter)
	}
	return approvers
}

func approvalChannelSlice(adapters []approvalAdapter) []approval.Channel {
	channels := make([]approval.Channel, 0, len(adapters))
	for _, adapter := range adapters {
		channels = append(channels, adapter)
	}
	return channels
}

func newOwnerRoute(adapters []approvalAdapter, routed func(ctx context.Context, result string)) (*approval.OwnerRoute, error) {
	return approval.NewOwnerRoute(approval.NewOwnerRouter(ownerApprovers(adapters), routed))
}

func newWorkflowRoute(adapters []approvalAdapter, routed func(ctx context.Context, result string)) (*approval.OwnerRoute, error) {
	return approval.NewWorkflowRoute(ownerApprovers(adapters), routed)
}

func approvalChannels(adapters []approvalAdapter, routes ...*approval.OwnerRoute) []approval.Channel {
	return approval.Targets(approvalChannelSlice(adapters), routes...)
}

func newEmailHandoffNotifier(adapters []approvalAdapter) email.HandoffNotifier {
	notifiers := make([]ownerHandoffNotifier, 0, len(adapters))
	for _, adapter := range adapters {
		notifiers = append(notifiers, adapter)
	}
	return &emailOwnerHandoffRouter{router: newOwnerHandoffRouter(notifiers)}
}

type channelBuildContext struct {
	cfg          *appconfig.Config
	router       *auraagent.Router
	db           *sql.DB
	artifactRoot string
	owners       *principal.Resolver
	principalRec *telemetry.PrincipalRecorder
	accessRec    *telemetry.AccessRecorder
	ingressRec   *telemetry.IngressRecorder
	runs         *erasureRuns
	commands     *command.Dispatcher
	logger       *slog.Logger
}

type serverChannelResult struct {
	ports          []runtimechannelhost.ChannelPort
	checks         []health.RegisteredCheck
	approvers      []approvalAdapter
	emailMailboxes []*email.Adapter
	ownerRoute     *approval.OwnerRoute
	workflowRoute  *approval.OwnerRoute
	accessRecorder *telemetry.AccessRecorder
}

func buildServerChannels(cfg *appconfig.Config, router *auraagent.Router, db *sql.DB, artifactRoot string, mp metric.MeterProvider, runs *erasureRuns, commands *command.Dispatcher, logger *slog.Logger) (serverChannelResult, error) {
	accessRecorder, err := telemetry.NewAccessRecorder(mp)
	if err != nil {
		return serverChannelResult{}, err
	}
	result := serverChannelResult{accessRecorder: accessRecorder}
	anyEnabled := cfg != nil && (cfg.Channels.Discord.Enabled || cfg.Channels.Telegram.Enabled || cfg.Channels.Email.Enabled)
	var bc *channelBuildContext
	if anyEnabled {
		bc, err = newChannelBuildContext(cfg, router, db, artifactRoot, mp, accessRecorder, runs, commands, logger)
		if err != nil {
			return serverChannelResult{}, err
		}
		discordPorts, discordChecks, discordApprovers, err := buildDiscordAdapters(bc)
		if err != nil {
			return serverChannelResult{}, err
		}
		result.ports = append(result.ports, discordPorts...)
		result.checks = append(result.checks, discordChecks...)
		result.approvers = append(result.approvers, discordApprovers...)

		telegramPorts, telegramChecks, telegramApprovers, err := buildTelegramAdapters(bc)
		if err != nil {
			return serverChannelResult{}, err
		}
		result.ports = append(result.ports, telegramPorts...)
		result.checks = append(result.checks, telegramChecks...)
		result.approvers = append(result.approvers, telegramApprovers...)
	}
	if cfg != nil && (cfg.Channels.HTTP.Enabled || cfg.Channels.Email.Enabled) {
		result.ownerRoute, err = newOwnerRoute(result.approvers, accessRecorder.ApprovalRouted)
		if err != nil {
			return serverChannelResult{}, err
		}
	}
	if len(result.approvers) > 0 {
		result.workflowRoute, err = newWorkflowRoute(result.approvers, accessRecorder.ApprovalRouted)
		if err != nil {
			return serverChannelResult{}, err
		}
	}
	if bc != nil && cfg.Channels.Email.Enabled {
		emailPorts, emailChecks, mailboxes, err := buildEmailAdapters(bc, result.ownerRoute, newEmailHandoffNotifier(result.approvers))
		if err != nil {
			return serverChannelResult{}, err
		}
		result.ports = append(result.ports, emailPorts...)
		result.checks = append(result.checks, emailChecks...)
		result.emailMailboxes = mailboxes
	}
	return result, nil
}

func newChannelBuildContext(cfg *appconfig.Config, router *auraagent.Router, db *sql.DB, artifactRoot string, mp metric.MeterProvider, accessRecorder *telemetry.AccessRecorder, runs *erasureRuns, commands *command.Dispatcher, logger *slog.Logger) (*channelBuildContext, error) {
	if router == nil {
		return nil, errors.New("persona router must not be nil")
	}
	owners, err := ownerResolver(cfg)
	if err != nil {
		return nil, err
	}
	principalRec, err := telemetry.NewPrincipalRecorder(mp)
	if err != nil {
		return nil, err
	}
	ingressRec, err := telemetry.NewIngressRecorder(mp)
	if err != nil {
		return nil, err
	}
	return &channelBuildContext{
		cfg:          cfg,
		router:       router,
		db:           db,
		artifactRoot: artifactRoot,
		owners:       owners,
		principalRec: principalRec,
		accessRec:    accessRecorder,
		ingressRec:   ingressRec,
		runs:         runs,
		commands:     commands,
		logger:       logger,
	}, nil
}

func buildDiscordAdapter(cfg *appconfig.Config, account *discord.Account, db *sql.DB, artifactRoot string, deps *discord.Deps) (*discord.Adapter, error) {
	executor, err := newChannelEffects(db, deps.Logger)
	if err != nil {
		return nil, err
	}
	mediaStore, err := media.NewArtifactStore(storage.NewArtifactStore(db, artifactRoot, int64(cfg.Storage.ArtifactQuota)))
	if err != nil {
		return nil, err
	}
	sessions, err := access.NewSessionEnsurer(turnstore.NewSessionService(db))
	if err != nil {
		return nil, err
	}
	resolver, err := access.NewConversationResolver(turnstore.NewConversationSessionStore(db))
	if err != nil {
		return nil, err
	}
	resumes, err := discord.NewSQLiteResumeStore(db, account.Name)
	if err != nil {
		return nil, err
	}
	full := *deps
	full.Config = &cfg.Channels.Discord
	full.Account = account
	full.Resumes = resumes
	full.Effects = executor
	full.Media = mediaStore
	full.Sessions = sessions
	full.SessionResolver = resolver
	full.SecretVault = buildSecretVault(cfg)
	full.Vision = newVisionService(cfg, db, artifactRoot)
	return discord.New(&full)
}

func buildDiscordAdapters(bc *channelBuildContext) ([]runtimechannelhost.ChannelPort, []health.RegisteredCheck, []approvalAdapter, error) {
	if !bc.cfg.Channels.Discord.Enabled {
		return nil, nil, nil, nil
	}
	adapters := make(discord.Adapters, 0, len(bc.cfg.Channels.Discord.Accounts))
	ports := make([]runtimechannelhost.ChannelPort, 0, len(bc.cfg.Channels.Discord.Accounts))
	approvers := make([]approvalAdapter, 0, len(bc.cfg.Channels.Discord.Accounts))
	for i := range bc.cfg.Channels.Discord.Accounts {
		settings := channelAccessSettings(bc.cfg, bc.db, bc.accessRec, bc.runs)
		adapter, err := buildDiscordAdapter(bc.cfg, &bc.cfg.Channels.Discord.Accounts[i], bc.db, bc.artifactRoot, &discord.Deps{
			Logger:            bc.logger,
			PrincipalResolver: bc.owners,
			PersonaRouter:     bc.router,
			Access:            &settings,
			Commands:          bc.commands,
			OnAdmitted: func(ctx context.Context, class principal.Class) {
				bc.principalRec.Admitted(ctx, string(class), principal.ChannelDiscord)
			},
			OnDropped: func(ctx context.Context, reason string) {
				bc.ingressRec.Dropped(ctx, principal.ChannelDiscord, reason)
			},
		})
		if err != nil {
			return nil, nil, nil, err
		}
		adapters = append(adapters, adapter)
		ports = append(ports, adapter)
		approvers = append(approvers, adapter)
	}
	timeout := time.Duration(bc.cfg.Health.CheckTimeout)
	checks := make([]health.RegisteredCheck, 0, len(adapters.HealthChecks())+1)
	for _, check := range adapters.HealthChecks() {
		if check.Timeout == 0 {
			check.Timeout = timeout
		}
		checks = append(checks, check)
	}
	checks = append(checks, health.RegisteredCheck{
		ID:          "principal",
		Checker:     identityMappingCheck{owners: bc.owners},
		Timeout:     timeout,
		Remediation: remediationMapOwnerIdentity,
	})
	return ports, checks, approvers, nil
}

func buildTelegramAdapters(bc *channelBuildContext) ([]runtimechannelhost.ChannelPort, []health.RegisteredCheck, []approvalAdapter, error) {
	if !bc.cfg.Channels.Telegram.Enabled {
		return nil, nil, nil, nil
	}
	adapters := make(telegramAdapters, 0, len(bc.cfg.Channels.Telegram.Accounts))
	ports := make([]runtimechannelhost.ChannelPort, 0, len(bc.cfg.Channels.Telegram.Accounts))
	approvers := make([]approvalAdapter, 0, len(bc.cfg.Channels.Telegram.Accounts))
	for i := range bc.cfg.Channels.Telegram.Accounts {
		adapter, err := newTelegramAdapter(bc.cfg, &bc.cfg.Channels.Telegram.Accounts[i], bc.db, bc.artifactRoot, bc.logger)
		if err != nil {
			return nil, nil, nil, err
		}
		adapter.SetPrincipalResolver(bc.owners)
		adapter.SetPersonaRouter(bc.router)
		settings := telegramAccessSettings(bc.cfg, bc.db, bc.accessRec, bc.runs)
		adapter.SetAccess(&settings)
		adapter.SetAdmissionObserver(func(ctx context.Context, class principal.Class) {
			bc.principalRec.Admitted(ctx, string(class), principal.ChannelTelegram)
		})
		adapter.SetDenialObserver(bc.accessRec.Denied)
		adapter.SetDropObserver(func(ctx context.Context, reason string) {
			bc.ingressRec.Dropped(ctx, principal.ChannelTelegram, reason)
		})
		if bc.commands != nil {
			adapter.SetCommands(bc.commands)
		}
		adapters = append(adapters, adapter)
		ports = append(ports, adapter)
		approvers = append(approvers, adapter)
	}
	checks := []health.RegisteredCheck{
		{
			ID:          "telegram",
			Checker:     adapters,
			Timeout:     time.Duration(bc.cfg.Health.CheckTimeout),
			Remediation: telegram.RemediationReviewBot,
		},
	}
	return ports, checks, approvers, nil
}

func buildEmailAdapters(bc *channelBuildContext, ownerRoute *approval.OwnerRoute, handoffs email.HandoffNotifier) ([]runtimechannelhost.ChannelPort, []health.RegisteredCheck, []*email.Adapter, error) {
	if !bc.cfg.Channels.Email.Enabled {
		return nil, nil, nil, nil
	}
	cursors, err := email.NewSQLiteCursorStore(bc.db)
	if err != nil {
		return nil, nil, nil, err
	}
	sessions, err := access.NewSessionEnsurer(turnstore.NewSessionService(bc.db))
	if err != nil {
		return nil, nil, nil, err
	}
	mediaStore, err := media.NewArtifactStore(storage.NewArtifactStore(bc.db, bc.artifactRoot, int64(bc.cfg.Storage.ArtifactQuota)))
	if err != nil {
		return nil, nil, nil, err
	}
	visionSvc := newVisionService(bc.cfg, bc.db, bc.artifactRoot)
	adapters := make(email.Adapters, 0, len(bc.cfg.Channels.Email.Accounts))
	ports := make([]runtimechannelhost.ChannelPort, 0, len(bc.cfg.Channels.Email.Accounts))
	for i := range bc.cfg.Channels.Email.Accounts {
		account := &bc.cfg.Channels.Email.Accounts[i]
		connector, err := email.NewIMAPConnector(account)
		if err != nil {
			return nil, nil, nil, err
		}
		mailer, err := email.NewSMTPMailer(account)
		if err != nil {
			return nil, nil, nil, err
		}
		executor, err := newChannelEffects(bc.db, bc.logger)
		if err != nil {
			return nil, nil, nil, err
		}
		settings := channelAccessSettings(bc.cfg, bc.db, bc.accessRec, bc.runs)
		adapter, err := email.New(&email.Deps{
			Config:            &bc.cfg.Channels.Email,
			Account:           account,
			Connector:         connector,
			Mailer:            mailer,
			Cursors:           cursors,
			Effects:           executor,
			Sessions:          sessions,
			Logger:            bc.logger,
			PrincipalResolver: bc.owners,
			PersonaRouter:     bc.router,
			Access:            &email.AccessSettings{Budgets: settings.Budgets, Evals: settings.Evals, Handoffs: settings.Handoffs, Erasure: settings.Erasure},
			OwnerRoute:        ownerRoute,
			HandoffNotifier:   handoffs,
			Media:             mediaStore,
			Vision:            visionSvc,
			OnAdmitted: func(ctx context.Context, class principal.Class) {
				bc.principalRec.Admitted(ctx, string(class), principal.ChannelEmail)
			},
			OnDropped: func(ctx context.Context, reason string) {
				bc.ingressRec.Dropped(ctx, principal.ChannelEmail, reason)
			},
		})
		if err != nil {
			return nil, nil, nil, err
		}
		adapters = append(adapters, adapter)
		ports = append(ports, adapter)
	}
	checks := adapters.HealthChecks()
	for i := range checks {
		if checks[i].Timeout == 0 {
			checks[i].Timeout = time.Duration(bc.cfg.Health.CheckTimeout)
		}
	}
	return ports, checks, []*email.Adapter(adapters), nil
}

func recoverEmailEffects(ctx context.Context, cfg *appconfig.Config, db *sql.DB, logger *slog.Logger) error {
	if !cfg.Channels.Email.Enabled {
		return nil
	}
	executor, err := newChannelEffects(db, logger)
	if err != nil {
		return err
	}
	return email.RecoverEffects(ctx, executor.Journal(), logger)
}
