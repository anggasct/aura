package app

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"maps"
	"math"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/metric"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/channels/discord"
	"github.com/anggasct/aura/internal/channels/email"
	"github.com/anggasct/aura/internal/channels/httpchat"
	"github.com/anggasct/aura/internal/channels/kit/access"
	"github.com/anggasct/aura/internal/channels/kit/approval"
	"github.com/anggasct/aura/internal/channels/kit/media"
	"github.com/anggasct/aura/internal/channels/telegram"
	"github.com/anggasct/aura/internal/channels/terminal"
	"github.com/anggasct/aura/internal/modules/broadcast"
	"github.com/anggasct/aura/internal/modules/codeexec"
	personaeval "github.com/anggasct/aura/internal/modules/eval/persona"
	"github.com/anggasct/aura/internal/modules/fetch"
	"github.com/anggasct/aura/internal/modules/filesystem"
	"github.com/anggasct/aura/internal/modules/github"
	"github.com/anggasct/aura/internal/modules/handoff"
	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/modules/mcp"
	"github.com/anggasct/aura/internal/modules/memory"
	"github.com/anggasct/aura/internal/modules/plugin"
	"github.com/anggasct/aura/internal/modules/plugin/auth"
	"github.com/anggasct/aura/internal/modules/profile"
	"github.com/anggasct/aura/internal/modules/schedule"
	"github.com/anggasct/aura/internal/modules/search"
	"github.com/anggasct/aura/internal/modules/shellexec"
	"github.com/anggasct/aura/internal/modules/skills"
	"github.com/anggasct/aura/internal/modules/workflow"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	policyapproval "github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability"
	"github.com/anggasct/aura/internal/platform/clikit"
	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/durable"
	durablesqlite "github.com/anggasct/aura/internal/platform/durable/sqlite"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/egress"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/secret"
	"github.com/anggasct/aura/internal/platform/server"
	"github.com/anggasct/aura/internal/platform/storage"
	"github.com/anggasct/aura/internal/platform/telemetry"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeadk "github.com/anggasct/aura/internal/platform/turn/adk"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
	runtimeengine "github.com/anggasct/aura/internal/platform/turn/engine"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
	"github.com/anggasct/aura/internal/platform/usage"
)

const (
	telegramWebhookDrainDivisor  = 2
	httpChatDrainDivisor         = 2
	conversationSessionSeparator = "~"
	knowledgeOwnerActionKind     = "kb.owner_action.v1"
	handoffReplyAuthorFallback   = "aura"
	legacyCronPrincipal          = "cron"
	retentionSweepInterval       = 24 * time.Hour
	erasureRunPoll               = 500 * time.Millisecond
	erasureRunBound              = 30 * time.Second
	judgeMaxOutputTokens         = 512
)

var (
	deliverText         = discord.DeliverText
	deliverTelegramText = telegram.DeliverText
	deliverEmailText    = email.DeliverText
)

type turnHookFunc func(ctx context.Context, req *turn.TurnRequest, events []turn.Event) error

func (f turnHookFunc) AfterTurn(ctx context.Context, req *turn.TurnRequest, events []turn.Event) error {
	return f(ctx, req, events)
}

func composeTurnHooks(hooks []turn.TurnHook) func(ctx context.Context, req *turn.TurnRequest, events []turn.Event) error {
	active := slices.DeleteFunc(slices.Clone(hooks), func(hook turn.TurnHook) bool { return hook == nil })
	switch len(active) {
	case 0:
		return nil
	case 1:
		return active[0].AfterTurn
	}
	return func(ctx context.Context, req *turn.TurnRequest, events []turn.Event) error {
		errs := make([]error, 0, len(active))
		for _, hook := range active {
			errs = append(errs, hook.AfterTurn(ctx, req, events))
		}
		return errors.Join(errs...)
	}
}

type moduleParts struct {
	skillRenderer runtimeengine.SkillRenderer
	skills        command.Skills
}

func (b *build) buildModules(ctx context.Context) error {
	steps := []func(context.Context) error{
		b.buildBuiltinTools,
		b.buildMemory,
		b.buildProfile,
		b.buildSkills,
		b.buildKnowledge,
		b.buildGitHub,
		b.buildInterpreter,
		b.buildServerWorkflows,
		b.buildPersonaEval,
	}
	for _, step := range steps {
		if err := step(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (b *build) buildBuiltinTools(context.Context) error {
	if b.broker == nil || b.cfg.Tools == nil {
		return nil
	}
	toolsCfg := b.cfg.Tools
	shellMod, err := shellexec.New(&shellexec.Deps{
		Workspace: toolsCfg.Workspace,
		Config:    toolsCfg.Exec,
	})
	if err != nil {
		return err
	}
	if err := b.registerModule("shellexec", shellMod); err != nil {
		return err
	}
	fsMod, err := filesystem.New(&filesystem.Deps{
		Workspace: toolsCfg.Workspace,
	})
	if err != nil {
		return err
	}
	if err := b.registerModule("filesystem", fsMod); err != nil {
		return err
	}
	fetchMod, err := fetch.New(&fetch.Deps{
		Config: &toolsCfg.Fetch,
	})
	if err != nil {
		return err
	}
	if err := b.registerModule("fetch", fetchMod); err != nil {
		return err
	}
	searchMod, err := search.New(&search.Deps{
		Config:       &toolsCfg.WebSearch,
		Timeout:      time.Duration(toolsCfg.Fetch.Timeout),
		MaxBodyBytes: toolsCfg.Fetch.MaxDecodedBytes,
	})
	if err != nil {
		return err
	}
	return b.registerModule("search", searchMod)
}

func (b *build) buildServerWorkflows(ctx context.Context) error {
	if !b.onServer() {
		return nil
	}
	return b.buildWorkflows(ctx, true)
}

func (b *build) addHealthCheck(check *health.RegisteredCheck) {
	if b.onServer() {
		b.server.checks = append(b.server.checks, *check)
	}
}

func (b *build) registerModule(name string, mod any) error {
	if tp, ok := mod.(toolbroker.ToolProvider); ok && b.broker != nil {
		if err := b.broker.RegisterProvider(tp); err != nil {
			return err
		}
	}
	if cp, ok := mod.(turn.ContextProvider); ok {
		b.execOpts = append(b.execOpts, runtimeadk.WithContextProvider(cp))
	}
	if th, ok := mod.(turn.TurnHook); ok {
		b.turnHooks = append(b.turnHooks, th)
	}
	if hp, ok := mod.(health.HealthProvider); ok {
		for _, check := range hp.HealthChecks() {
			if check.Timeout == 0 {
				check.Timeout = time.Duration(b.cfg.Health.CheckTimeout)
			}
			b.addHealthCheck(&check)
		}
	}
	if provider, ok := mod.(operation.Provider); ok {
		b.bindings = append(b.bindings, provider.Bindings()...)
	}
	if provider, ok := mod.(operation.BehaviorProvider); ok {
		b.behaviors = append(b.behaviors, provider.Behaviors()...)
	}
	switch m := mod.(type) {
	case server.Lifecycle:
		if err := b.group.Add(m); err != nil {
			return err
		}
	case io.Closer:
		if err := b.group.Defer(name, func(context.Context) error {
			return m.Close()
		}); err != nil {
			return err
		}
	}
	return nil
}

func (b *build) buildMemory(ctx context.Context) error {
	cfg, logger := b.cfg, b.logger
	if !operationGroupEnabled(cfg, memory.GroupName) {
		return nil
	}
	var observer memory.Observer
	if b.onServer() {
		recorder, err := telemetry.NewMemoryRecorder(b.meters())
		if err != nil {
			return err
		}
		observer = memoryRecorderObserver(recorder)
	}
	mod, err := memory.New(ctx, &memory.Deps{
		Config:      cfg.Memory,
		DB:          b.db,
		Sessions:    turnstore.NewSessionService(b.db),
		Scopes:      b.registry,
		Secrets:     secret.New(),
		Models:      b.models,
		ModelConfig: &cfg.Models,
		ModelRoutes: cfg.ModelRoutes,
		Observer:    observer,
		Logger:      logger,
	})
	if err != nil {
		return err
	}
	if err := b.registerModule("memory", mod); err != nil {
		return err
	}
	if !b.onServer() {
		return nil
	}
	if loop := mod.PurgeLoop(); loop != nil {
		return b.group.Add(server.Background("memory_purge", loop))
	}
	return nil
}

func memoryRecorderObserver(recorder *telemetry.MemoryRecorder) memory.Observer {
	if recorder == nil {
		return nil
	}
	return func(ctx context.Context, observation *memory.Observation) {
		recorder.Record(ctx, &telemetry.MemoryObservation{
			Outcome:     observation.Outcome,
			Summarized:  observation.Summarized,
			Documents:   observation.Documents,
			Screened:    observation.Screened,
			ModelSystem: observation.ModelProtocol,
			Duration:    observation.Duration,
		})
	}
}

func (b *build) buildProfile(ctx context.Context) error {
	cfg := b.cfg
	if !operationGroupEnabled(cfg, profile.GroupName) {
		return nil
	}
	mod, err := profile.New(ctx, &profile.Deps{
		Config:       *cfg.Profile,
		DB:           b.db,
		Sessions:     turnstore.NewSessionService(b.db),
		Events:       turnstore.NewEventStore(b.db),
		Models:       b.models,
		ModelConfig:  cfg.Models,
		ModelRoutes:  cfg.ModelRoutes,
		Scanner:      appconfig.BuildSecretVault(cfg),
		Owner:        cfg.Owner,
		Meters:       b.meters(),
		CheckTimeout: time.Duration(cfg.Health.CheckTimeout),
		Logger:       b.logger,
	})
	if err != nil {
		return err
	}
	if err := b.registerModule("profile", mod); err != nil {
		return err
	}
	return b.group.Add(server.Background("profile_extraction", mod.Run))
}

func (b *build) buildSkills(ctx context.Context) error {
	if b.cfg.Skills == nil || !b.cfg.Skills.Enabled {
		return nil
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
	if err := b.registerModule("skills", mod); err != nil {
		return err
	}
	b.modules.skillRenderer = mod.Renderer()
	b.modules.skills = mod.ChatCommands()
	if b.onServer() && b.server.commandSkills != nil {
		b.server.commandSkills.bind(b.modules.skills)
	}
	return nil
}

type skillActivationLog struct {
	events turnstore.EventKindReader
}

func (l *skillActivationLog) Activations(ctx context.Context, sessionID string, limit int) ([]turn.SkillActivation, error) {
	events, err := l.events.LatestByKind(ctx, sessionID, turn.EventKindSkillActivated, limit)
	if err != nil {
		return nil, err
	}
	out := make([]turn.SkillActivation, 0, len(events))
	for i := range events {
		var activation turn.SkillActivation
		if err := json.Unmarshal(events[i].Payload, &activation); err != nil || activation.SkillID == "" {
			continue
		}
		out = append(out, activation)
	}
	return out, nil
}

func (b *build) buildKnowledgeOperations() error {
	service, err := newKnowledgeService(b.cfg, b.db, nil)
	if err != nil {
		return err
	}
	operations, err := knowledge.NewOperations(&knowledge.OperationDeps{Service: service, Logger: b.logger})
	if err != nil {
		return err
	}
	return b.registerModule("knowledge_operations", operations)
}

func (b *build) buildKnowledge(ctx context.Context) error {
	if b.onServer() {
		if err := b.buildKnowledgeOperations(); err != nil {
			return err
		}
	}
	scopes := personaKnowledgeScopes(b.cfg.Agents)
	if len(scopes) == 0 {
		if b.cfg.Knowledge != nil && b.cfg.Knowledge.Embedding != nil {
			checks, err := knowledgeStatusChecks(b.cfg)
			if err != nil {
				return err
			}
			for i := range checks {
				b.addHealthCheck(&checks[i])
			}
		}
		return nil
	}
	dbPath, _, _, err := storage.ResolvePaths(&b.cfg.Storage)
	if err != nil {
		return err
	}
	mod, err := knowledge.New(ctx, &knowledge.ModuleDeps{
		Config:       b.cfg.Knowledge,
		DBPath:       dbPath,
		Scopes:       scopes,
		Secrets:      &knowledgeSecrets{cfg: b.cfg},
		CheckTimeout: time.Duration(b.cfg.Health.CheckTimeout),
		Logger:       b.logger,
	})
	if err != nil {
		return err
	}
	if err := b.registerModule("knowledge", mod); err != nil {
		_ = mod.Close()
		return err
	}
	if run := mod.VectorLoader(); run != nil {
		if err := b.group.Add(server.Background("knowledge_vectors", run)); err != nil {
			return err
		}
	}
	return nil
}

func (b *build) buildGitHub(context.Context) error {
	mod, err := github.New(&github.ModuleDeps{Config: githubConfig(b.cfg)})
	if err != nil {
		return err
	}
	return b.registerModule("github", mod)
}

func (b *build) buildInterpreter(ctx context.Context) error {
	cfg, logger := b.cfg, b.logger
	if b.broker == nil || !interpreterEnabled(b.result.CapabilityReport) {
		return nil
	}
	dataRoot, err := storage.ResolveDataRoot(&cfg.Storage)
	if err != nil {
		return err
	}
	var observer codeexec.Observer
	if b.onServer() {
		recorder, err := telemetry.NewCodeRecorder(b.meters())
		if err != nil {
			return err
		}
		observer = codeObserver(recorder)
	}
	mod, err := codeexec.New(ctx, &codeexec.ModuleDeps{
		Config:       &cfg.Interpreter,
		DataRoot:     dataRoot,
		Artifacts:    b.artifacts,
		Observer:     observer,
		CheckTimeout: time.Duration(cfg.Health.CheckTimeout),
		Logger:       logger,
	})
	if err != nil {
		return err
	}
	if err := b.registerModule("interpreter", mod); err != nil {
		return err
	}
	return b.group.Add(server.Background("interpreter_verification", mod.VerifyRun))
}

func (b *build) buildWorkflows(ctx context.Context, host bool) error {
	cfg, db := b.cfg, b.db
	deps, err := workflowValidationDeps(cfg)
	if err != nil {
		return err
	}
	runtime, err := b.durableRuntime()
	if err != nil {
		return err
	}
	modDeps := &workflow.ModuleDeps{
		DB:             db,
		Sessions:       b.turns,
		Durable:        runtime,
		Config:         cfg.Workflows,
		ValidationDeps: deps,
		Logger:         b.logger,
	}
	if host && b.broker != nil {
		modDeps.Broker = b.broker
		modDeps.RunSessions = b.sessions
		modDeps.Registry = b.registry
		modDeps.Appender = &workflowSessionTextAppender{db: db}
		modDeps.Deliver = &workflowCompletionDeliverer{cfg: cfg, deliver: deliverText, deliverTelegram: deliverTelegramText}
		modDeps.CheckTimeout = time.Duration(cfg.Health.CheckTimeout)
		if b.server != nil {
			modDeps.Route = newWorkflowOwnerRoute(b.server.workflowRoute)
			modDeps.Targets = workflowReplyTargeters(b.server.channels)
			modDeps.Offerers = workflowProposalOfferers(b.server.channels)
		}
	}
	mod, err := workflow.New(ctx, modDeps)
	if err != nil {
		return err
	}
	b.workflowMod = mod
	b.quarantined = mod.Quarantined()
	b.interpreter = mod.Interpreter()
	if host && b.onServer() {
		if err := b.registerModule("workflow", mod); err != nil {
			return err
		}
	}
	return nil
}

func (b *build) buildPersonaEval(_ context.Context) error {
	if !b.onServer() {
		return nil
	}
	capturer := newPersonaCapturer(b.cfg)
	models := behaviorModels{defaultModel: b.primaryModel(), route: b.routeModel, models: b.models, toolBudget: toolBudgetFromRuntime(b.cfg.Runtime)}
	mod, err := personaeval.New(&personaeval.ModuleDeps{
		DB:           b.db,
		Registry:     b.registry,
		ConfigPath:   b.result.Path,
		Capturer:     capturer.capture,
		Converse:     models.converse,
		JudgeFor:     models.judge,
		Catalog:      appconfig.BuiltinToolDefinitions(),
		ParseCron:    parsePersonaEvalCron,
		CheckTimeout: time.Duration(b.cfg.Health.CheckTimeout),
		Logger:       b.logger,
	})
	if err != nil {
		return err
	}
	b.personaEvalMod = mod
	return b.registerModule("persona_eval", mod)
}

func parsePersonaEvalCron(expr string) (func(loc *time.Location, now time.Time) (time.Time, error), error) {
	parsed, err := schedule.ParseExpression(expr)
	if err != nil {
		return nil, err
	}
	return func(loc *time.Location, now time.Time) (time.Time, error) {
		return schedule.NextFire(&parsed, loc, now)
	}, nil
}

func personaToolNames(cfg *appconfig.Config) []string {
	names := append(definitionNames(), operationToolNames()...)
	if len(githubCredentials(cfg)) > 0 {
		names = append(names, github.ToolNames()...)
	}
	return names
}

func buildAgentRegistry(cfg *appconfig.Config) (*auraagent.Registry, error) {
	routeSet := make(map[string]bool, len(cfg.Models.Definitions)+len(cfg.ModelRoutes))
	for route := range cfg.Models.Definitions {
		routeSet[route] = true
	}
	for route := range cfg.ModelRoutes {
		routeSet[route] = true
	}
	modelRoutes := slices.Collect(maps.Keys(routeSet))
	slices.Sort(modelRoutes)
	var overrides []auraagent.Persona
	if cfg.Agents != nil {
		overrides = cfg.Agents.Definitions
	}
	catalog := append(appconfig.BuiltinToolDefinitions(), github.Definitions()...)
	deploymentLanguage := ""
	if cfg.Owner != nil {
		deploymentLanguage = cfg.Owner.Language
	}
	registry, err := auraagent.BuildWithMCP(overrides, personaToolNames(cfg), catalog, cfg.MCP.ServerNames(), modelRoutes, deploymentLanguage)
	if err != nil {
		return nil, err
	}
	definitions := registry.Definitions()
	for i := range definitions {
		behavior := definitions[i].EvalBehavior
		if behavior == nil || behavior.Schedule == "" {
			continue
		}
		if _, err := schedule.ParseExpression(behavior.Schedule); err != nil {
			return nil, fault.New(auraagent.ErrorCodeDefinitionInvalid, fmt.Sprintf("agent %q evals.behavior.schedule: %v", definitions[i].ID, err))
		}
	}
	return registry, nil
}

func channelAccounts(channels *appconfig.Channels) []auraagent.ChannelAccount {
	accounts := make([]auraagent.ChannelAccount, 0, len(channels.Discord.Accounts)+len(channels.Telegram.Accounts)+len(channels.Email.Accounts))
	for i := range channels.Discord.Accounts {
		account := &channels.Discord.Accounts[i]
		accounts = append(accounts, auraagent.ChannelAccount{
			Channel: auraagent.BindingChannelDiscord,
			Name:    account.Name,
			Persona: account.Persona,
			Field:   fmt.Sprintf("channels.discord.accounts[%d]", i),
		})
	}
	for i := range channels.Telegram.Accounts {
		account := &channels.Telegram.Accounts[i]
		accounts = append(accounts, auraagent.ChannelAccount{
			Channel: auraagent.BindingChannelTelegram,
			Name:    account.Name,
			Persona: account.Persona,
			Field:   fmt.Sprintf("channels.telegram.accounts[%d]", i),
		})
	}
	for i := range channels.Email.Accounts {
		account := &channels.Email.Accounts[i]
		accounts = append(accounts, auraagent.ChannelAccount{
			Channel: auraagent.BindingChannelEmail,
			Name:    account.Name,
			Persona: account.Persona,
			Field:   fmt.Sprintf("channels.email.accounts[%d]", i),
		})
	}
	return accounts
}

func ownerResolver(cfg *appconfig.Config) (*principal.Resolver, error) {
	var identities []string
	if cfg != nil && cfg.Owner != nil {
		identities = cfg.Owner.Identities
	}
	return principal.NewResolver(identities)
}

func ownerReachableInChat(cfg *appconfig.Config, owners *principal.Resolver) bool {
	if cfg == nil {
		return false
	}
	if cfg.Channels.Telegram.Enabled && len(cfg.Channels.Telegram.Accounts) > 0 && owners.Count(principal.ChannelTelegram) > 0 {
		return true
	}
	if !cfg.Channels.Discord.Enabled || owners.Count(principal.ChannelDiscord) == 0 {
		return false
	}
	accounts := cfg.Channels.Discord.Accounts
	for i := range accounts {
		if accounts[i].AcceptDMs {
			return true
		}
	}
	return false
}

func allowlistedPrincipals(cfg *appconfig.Config, owners *principal.Resolver) []principal.AllowlistedPrincipal {
	var out []principal.AllowlistedPrincipal
	if cfg == nil {
		return out
	}
	for i := range cfg.Channels.Discord.Accounts {
		account := &cfg.Channels.Discord.Accounts[i]
		for _, userID := range account.AllowedUserIDs {
			if owners.IsOwnerIdentity(principal.ChannelDiscord, userID) {
				continue
			}
			out = append(out, principal.AllowlistedPrincipal{
				PrincipalID: principal.Namespaced(principal.ChannelDiscord, userID),
				Account:     account.Name,
			})
		}
	}
	for i := range cfg.Channels.Telegram.Accounts {
		account := &cfg.Channels.Telegram.Accounts[i]
		for _, userID := range account.AllowedUserIDs {
			if owners.IsOwnerIdentity(principal.ChannelTelegram, userID) {
				continue
			}
			out = append(out, principal.AllowlistedPrincipal{
				PrincipalID: principal.Namespaced(principal.ChannelTelegram, userID),
				Account:     account.Name,
			})
		}
	}
	for i := range cfg.Channels.Email.Accounts {
		account := &cfg.Channels.Email.Accounts[i]
		for _, address := range account.AllowedSenders {
			if owners.IsOwnerIdentity(principal.ChannelEmail, address) {
				continue
			}
			out = append(out, principal.AllowlistedPrincipal{
				PrincipalID: principal.Namespaced(principal.ChannelEmail, address),
				Account:     account.Name,
			})
		}
	}
	return out
}

func ownerRekeyMoves(cfg *appconfig.Config, osUser string) ([]principal.OwnerMove, error) {
	owners, err := ownerResolver(cfg)
	if err != nil {
		return nil, err
	}
	var moves []principal.OwnerMove
	seen := map[string]bool{principal.OwnerID: true}
	add := func(from, to string) {
		if from == "" || seen[from] {
			return
		}
		seen[from] = true
		moves = append(moves, principal.OwnerMove{From: from, To: to})
	}
	add(strings.TrimSpace(osUser), principal.OwnerID)
	add(legacyCronPrincipal, principal.OwnerID)
	if cfg != nil && cfg.Owner != nil {
		for _, identity := range cfg.Owner.Identities {
			if _, externalID, err := principal.ParseIdentity(identity); err == nil {
				add(externalID, principal.OwnerID)
			}
		}
	}
	if cfg != nil {
		for _, id := range discordAllowedUserIDs(&cfg.Channels.Discord) {
			if !owners.IsOwnerIdentity(principal.ChannelDiscord, id) {
				add(id, principal.Namespaced(principal.ChannelDiscord, id))
			}
		}
	}
	return moves, nil
}

func discordAllowedUserIDs(cfg *discord.Config) []string {
	var ids []string
	for i := range cfg.Accounts {
		ids = append(ids, cfg.Accounts[i].AllowedUserIDs...)
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

func rekeyParticipants(db *sql.DB) ([]principal.Rekeyer, error) {
	memoryRekeyer, err := memory.NewRekeyer(db)
	if err != nil {
		return nil, err
	}
	profileRekeyer, err := profile.NewRekeyer(db)
	if err != nil {
		return nil, err
	}
	return []principal.Rekeyer{
		turnstore.NewSessionRekeyer(db),
		memoryRekeyer,
		profileRekeyer,
	}, nil
}

func erasureParticipants(db *sql.DB) ([]principal.Eraser, error) {
	turns := turnstore.NewTurnStore(db)
	runs, err := durablesqlite.New(&durablesqlite.Options{DB: db})
	if err != nil {
		return nil, err
	}
	sessionApprovals, err := effect.NewSessionApprovalEraser(db, turns, false)
	if err != nil {
		return nil, err
	}
	runSessionApprovals, err := effect.NewSessionApprovalEraser(db, turns, true)
	if err != nil {
		return nil, err
	}
	orphanedTurns, err := runtimeengine.NewOrphanedTurnRunEraser(db, runs, turns)
	if err != nil {
		return nil, err
	}
	workflowEraser, err := workflow.NewEraser(db, turns, runs, sessionApprovals)
	if err != nil {
		return nil, err
	}
	memoryEraser, err := memory.NewEraser(db)
	if err != nil {
		return nil, err
	}
	return []principal.Eraser{
		workflowEraser,
		effect.NewEmailEffectEraser(db),
		memoryEraser,
		sessionApprovals,
		turnstore.NewSessionEraser(db, false),
		orphanedTurns,
		runSessionApprovals,
		turnstore.NewSessionEraser(db, true),
		principal.NewAccessEraser(db),
		usage.NewUsageLinkEraser(db),
	}, nil
}

func liveWorkflowRuns(ctx context.Context, db *sql.DB, principalID string) ([]string, error) {
	runs, err := durablesqlite.New(&durablesqlite.Options{DB: db})
	if err != nil {
		return nil, err
	}
	return workflow.LiveWorkflowRuns(ctx, db, principalID, turnstore.NewTurnStore(db), runs)
}

func collectErasedBlobs(ctx context.Context, db *sql.DB, artifactRoot string, before time.Time) (int, error) {
	collected, err := storage.Collect(ctx, db, artifactRoot, before)
	if err != nil {
		return 0, err
	}
	return collected.DeletedBlobs, nil
}

func erasePrincipal(ctx context.Context, db *sql.DB, scope principal.ErasureScope, artifactRoot string) (principal.ErasureReport, int, error) {
	if db == nil {
		return principal.EraseWithBlobs(ctx, nil, scope, nil, artifactRoot, collectErasedBlobs)
	}
	participants, err := erasureParticipants(db)
	if err != nil {
		return principal.ErasureReport{}, 0, err
	}
	return principal.EraseWithBlobs(ctx, db, scope, participants, artifactRoot, collectErasedBlobs)
}

type runCanceller interface {
	Cancel(ctx context.Context, runID string) error
}

type erasureRuns struct {
	canceller atomic.Pointer[runCanceller]
	live      func(ctx context.Context, db *sql.DB, principalID string) ([]string, error)
	poll      time.Duration
	bound     time.Duration
}

func (r *erasureRuns) set(canceller runCanceller) {
	if r != nil && canceller != nil {
		r.canceller.Store(&canceller)
	}
}

func (r *erasureRuns) settings() (live func(ctx context.Context, db *sql.DB, principalID string) ([]string, error), poll, bound time.Duration) {
	live, poll, bound = r.live, r.poll, r.bound
	if live == nil {
		live = liveWorkflowRuns
	}
	if poll <= 0 {
		poll = erasureRunPoll
	}
	if bound <= 0 {
		bound = erasureRunBound
	}
	return live, poll, bound
}

func (r *erasureRuns) stop(ctx context.Context, db *sql.DB, principalID string) ([]string, error) {
	if r == nil {
		return nil, nil
	}
	canceller := r.canceller.Load()
	if canceller == nil {
		return nil, nil
	}
	live, poll, bound := r.settings()
	runs, err := live(ctx, db, principalID)
	if err != nil || len(runs) == 0 {
		return nil, err
	}
	for _, id := range runs {
		if err := (*canceller).Cancel(ctx, id); err != nil && !errors.Is(err, durable.ErrUnknownRun) {
			return nil, fmt.Errorf("cancel workflow run %s: %w", id, err)
		}
	}
	deadline := time.NewTimer(bound)
	defer deadline.Stop()
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		remaining, err := live(ctx, db, principalID)
		if err != nil {
			return runs, err
		}
		if len(remaining) == 0 {
			return runs, nil
		}
		select {
		case <-ctx.Done():
			return runs, ctx.Err()
		case <-deadline.C:
			return runs, nil
		case <-ticker.C:
		}
	}
}

type liveRunCancellerAdapter struct {
	db   *sql.DB
	runs *erasureRuns
}

func (a *liveRunCancellerAdapter) CancelForPrincipal(ctx context.Context, principalID string) ([]string, error) {
	if a == nil || a.runs == nil {
		return nil, nil
	}
	return a.runs.stop(ctx, a.db, principalID)
}

func forgetPrincipal(ctx context.Context, db *sql.DB, runs *erasureRuns, scope principal.ErasureScope, artifactRoot string) (cancelled []string, err error) {
	var participants []principal.Eraser
	if db != nil {
		participants, err = erasureParticipants(db)
		if err != nil {
			return nil, err
		}
	}
	var liveRuns principal.LiveRunCanceller
	if runs != nil {
		liveRuns = &liveRunCancellerAdapter{db: db, runs: runs}
	}
	cancelled, _, err = principal.ForgetPrincipal(ctx, db, liveRuns, scope, participants, artifactRoot, collectErasedBlobs)
	return cancelled, err
}

type principalEraser struct {
	db           *sql.DB
	artifactRoot string
	now          func() time.Time
	runs         *erasureRuns
}

func (e *principalEraser) Forget(ctx context.Context, principalID string) error {
	if _, err := forgetPrincipal(ctx, e.db, e.runs, principal.ErasureScope{PrincipalID: principalID, Apply: true}, e.artifactRoot); err != nil {
		if fault.HasCode(err, principal.ErrorCodeErasureBlocked) {
			return fmt.Errorf("%w: %w", access.ErrForgetRunsStopping, err)
		}
		return err
	}
	channel, _, _ := strings.Cut(principalID, ":")
	return turnstore.NewOwnerActionRecorder(e.db).RecordOwnerAction(ctx, &principal.OwnerAction{
		Action:    principal.AccessActionPrincipalForget,
		Principal: principalID,
		Channel:   channel,
	}, "forgetme-"+principalID, e.now().UTC())
}

func sweepRetention(ctx context.Context, cfg *appconfig.Config, db *sql.DB, artifactRoot string, now time.Time) (map[principal.Class]int, error) {
	owners, err := ownerResolver(cfg)
	if err != nil {
		return nil, err
	}
	participants, err := erasureParticipants(db)
	if err != nil {
		return nil, err
	}
	swept, err := principal.SweepRetention(
		ctx, cfg.Access, db,
		allowlistedPrincipals(cfg, owners),
		turnStoreActivityReader{turns: turnstore.NewTurnStore(db)},
		participants, artifactRoot, collectErasedBlobs, now,
	)
	if err != nil {
		return swept, err
	}
	if _, err := operation.PruneReceipts(ctx, db, now.Add(-operation.ReceiptRetention)); err != nil {
		return swept, err
	}
	return swept, nil
}

func runRetentionSweeps(ctx context.Context, cfg *appconfig.Config, db *sql.DB, artifactRoot string, logger *slog.Logger) {
	sweep := func() {
		swept, err := sweepRetention(ctx, cfg, db, artifactRoot, time.Now().UTC())
		if err != nil {
			logger.WarnContext(ctx, "retention sweep failed", "component", "access", "error", err)
			return
		}
		logger.InfoContext(ctx, "retention sweep", "component", "access", "trusted", swept[principal.ClassTrusted], "public", swept[principal.ClassPublic])
	}
	sweep()
	ticker := time.NewTicker(retentionSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}

func channelAccessSettings(cfg *appconfig.Config, db *sql.DB, recorder *telemetry.AccessRecorder, runs *erasureRuns) access.Settings {
	var grants approval.StandingGrantRecorder
	if db != nil {
		grants = principal.NewStandingGrantRecorder(principal.NewStandingGrantStore(db), turnstore.NewOwnerActionRecorder(db))
	} else {
		grants = principal.NewStandingGrantRecorder(nil, nil)
	}
	var denied func(ctx context.Context, reason string)
	var approvalRouted func(ctx context.Context, result string)
	var onPairing principal.PairingObserver
	var onBudget usage.BudgetExhaustedObserver
	if recorder != nil {
		denied = recorder.Denied
		approvalRouted = recorder.ApprovalRouted
		onPairing = recorder.PairingRequest
		onBudget = recorder.BudgetExhausted
	}
	settings := access.Settings{
		Grants:         grants,
		Denied:         denied,
		ApprovalRouted: approvalRouted,
	}
	if db != nil {
		if pairings, err := principal.NewPairingStore(db, turnstore.NewTurnStore(db)); err == nil {
			if svc := principal.NewPairingService(pairings, principal.NewPrincipalGrantStore(db), onPairing); svc != nil {
				settings.Pairing = svc
			}
		}
	}
	if cfg.Access != nil {
		settings.ApprovalTimeout = time.Duration(cfg.Access.ApprovalTimeout)
		settings.StandingGrantTTL = time.Duration(cfg.Access.StandingGrantTTL)
	}
	if db != nil && cfg.Access != nil {
		if ledger, err := usage.NewLedger(db, usage.LedgerOptions{}); err == nil {
			if gate := usage.NewPrincipalBudgetGate(cfg.Access, ledger, onBudget); gate != nil {
				settings.Budgets = gate
			}
		}
	}
	if db != nil {
		_, artifactRoot, _, _ := storage.ResolvePaths(&cfg.Storage)
		settings.Erasure = &principalEraser{db: db, artifactRoot: artifactRoot, now: time.Now, runs: runs}
		if registry, err := buildAgentRegistry(cfg); err == nil {
			if evalStore, err := personaeval.NewSQLiteStore(db); err == nil {
				if gate, err := personaeval.NewGate(registry, evalStore); err == nil {
					settings.Evals = gate
				}
			}
		}
		if cfg.Access != nil {
			if handoffStore, err := handoff.NewSQLiteStore(db); err == nil {
				if gate, err := handoff.NewGate(handoffStore, time.Duration(cfg.Access.HandoffTimeout), time.Now); err == nil {
					settings.Handoffs = gate
				}
			}
		}
	}
	return settings
}

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
	full.SecretVault = appconfig.BuildSecretVault(cfg)
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
		Checker:     principal.IdentityMappingCheck{Owners: bc.owners},
		Timeout:     timeout,
		Remediation: principal.RemediationMapOwnerIdentity,
	})
	return ports, checks, approvers, nil
}

func buildTelegramAdapters(bc *channelBuildContext) ([]runtimechannelhost.ChannelPort, []health.RegisteredCheck, []approvalAdapter, error) {
	if !bc.cfg.Channels.Telegram.Enabled {
		return nil, nil, nil, nil
	}
	cursors, err := telegram.NewSQLiteCursorStore(bc.db)
	if err != nil {
		return nil, nil, nil, err
	}
	executor, err := newChannelEffects(bc.db, bc.logger)
	if err != nil {
		return nil, nil, nil, err
	}
	sessions, err := access.NewSessionEnsurer(turnstore.NewSessionService(bc.db))
	if err != nil {
		return nil, nil, nil, err
	}
	resolver, err := access.NewConversationResolver(turnstore.NewConversationSessionStore(bc.db))
	if err != nil {
		return nil, nil, nil, err
	}
	mediaStore, err := media.NewArtifactStore(storage.NewArtifactStore(bc.db, bc.artifactRoot, int64(bc.cfg.Storage.ArtifactQuota)))
	if err != nil {
		return nil, nil, nil, err
	}
	vault := appconfig.BuildSecretVault(bc.cfg)
	ledger := turnstore.NewDedupeStore(bc.db)
	visionSvc := newVisionService(bc.cfg, bc.db, bc.artifactRoot)
	settings := channelAccessSettings(bc.cfg, bc.db, bc.accessRec, bc.runs)
	adapters := make(telegram.Adapters, 0, len(bc.cfg.Channels.Telegram.Accounts))
	ports := make([]runtimechannelhost.ChannelPort, 0, len(bc.cfg.Channels.Telegram.Accounts))
	approvers := make([]approvalAdapter, 0, len(bc.cfg.Channels.Telegram.Accounts))
	for i := range bc.cfg.Channels.Telegram.Accounts {
		adapter, err := telegram.New(&telegram.Deps{
			Config:            &bc.cfg.Channels.Telegram,
			Account:           &bc.cfg.Channels.Telegram.Accounts[i],
			Cursors:           cursors,
			Effects:           executor,
			Sessions:          sessions,
			Logger:            bc.logger,
			SessionResolver:   resolver,
			PersonaRouter:     bc.router,
			PrincipalResolver: bc.owners,
			Commands:          bc.commands,
			SecretVault:       vault,
			Media:             mediaStore,
			Vision:            visionSvc,
			UpdateLedger:      ledger,
			Access:            &settings,
			OnAdmitted: func(ctx context.Context, class principal.Class) {
				bc.principalRec.Admitted(ctx, string(class), principal.ChannelTelegram)
			},
			OnDropped: func(ctx context.Context, reason string) {
				bc.ingressRec.Dropped(ctx, principal.ChannelTelegram, reason)
			},
		})
		if err != nil {
			return nil, nil, nil, err
		}
		adapters = append(adapters, adapter)
		ports = append(ports, adapter)
		approvers = append(approvers, adapter)
	}
	checks := adapters.HealthChecks()
	for i := range checks {
		if checks[i].Timeout == 0 {
			checks[i].Timeout = time.Duration(bc.cfg.Health.CheckTimeout)
		}
	}
	return ports, checks, approvers, nil
}

func buildTelegramWebhookServer(cfg *appconfig.Config, ports []runtimechannelhost.ChannelPort) (*telegram.WebhookServer, error) {
	adapters := make([]*telegram.Adapter, 0, len(ports))
	for _, port := range ports {
		if adapter, ok := port.(*telegram.Adapter); ok {
			adapters = append(adapters, adapter)
		}
	}
	handler, err := telegram.NewWebhookHandler(adapters)
	if err != nil {
		return nil, err
	}
	return telegram.NewWebhookServer(handler, cfg.Channels.Telegram.Webhook.ListenAddress, time.Duration(cfg.Runtime.ShutdownTimeout)/telegramWebhookDrainDivisor)
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

type emailRecoveryRetry struct {
	deps   *email.RecoveryDeps
	logger *slog.Logger
	retry  email.RecoveryRetry
}

func (r emailRecoveryRetry) pending() bool { return r.retry.Pending() }

func (r emailRecoveryRetry) run(ctx context.Context) {
	if err := email.RecoverEffectsAt(ctx, r.deps, r.retry); err != nil && ctx.Err() == nil {
		r.logger.ErrorContext(ctx, "email effect recovery retry failed", "component", "email", "error", err)
	}
}

func recoverEmailEffects(ctx context.Context, cfg *appconfig.Config, db *sql.DB, logger *slog.Logger) (emailRecoveryRetry, error) {
	if !cfg.Channels.Email.Enabled {
		return emailRecoveryRetry{}, nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	executor, err := newChannelEffects(db, logger)
	if err != nil {
		return emailRecoveryRetry{}, err
	}
	deps := &email.RecoveryDeps{Journal: executor.Journal(), Logger: logger, Clock: durable.RealClock()}
	retry, err := email.RecoverEffects(ctx, deps)
	if err != nil {
		return emailRecoveryRetry{}, err
	}
	return emailRecoveryRetry{deps: deps, logger: logger, retry: retry}, nil
}

type apiKeyDirectory struct {
	keys principal.APIKeyStore
}

func (d *apiKeyDirectory) GetByID(ctx context.Context, id string) (httpchat.Key, bool, error) {
	key, err := d.keys.GetByID(ctx, id)
	if code, ok := fault.CodeOf[principal.ErrorCode](err); ok && code == principal.ErrorCodeAPIKeyNotFound {
		return httpchat.Key{}, false, nil
	}
	if err != nil {
		return httpchat.Key{}, false, err
	}
	class, ok := principal.APIKeyClass(key.Class)
	if !ok {
		return httpchat.Key{}, false, fault.New(httpchat.ErrorCodeInvalidArgument, fmt.Sprintf("api key %s has an unknown class", key.ID))
	}
	return httpchat.Key{
		ID: key.ID, Digest: key.KeySHA256, PersonaID: key.PersonaID, Class: class,
		CreatedAt: key.CreatedAt, Revoked: !key.Active(),
	}, true, nil
}

type httpSessionStore struct {
	sessions turnstore.SessionService
}

func (s *httpSessionStore) Get(ctx context.Context, id string) (httpchat.Session, bool, error) {
	found, err := s.sessions.Get(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return httpchat.Session{}, false, nil
	}
	if code, ok := fault.CodeOf[turnstore.ErrorCode](err); ok && code == turnstore.ErrorCodeSessionNotFound {
		return httpchat.Session{}, false, nil
	}
	if err != nil {
		return httpchat.Session{}, false, err
	}
	return httpchat.Session{ID: found.ID, OwnerID: found.OwnerID, PersonaID: found.AgentID}, true, nil
}

func (s *httpSessionStore) Create(ctx context.Context, sess *httpchat.Session) error {
	now := time.Now().UTC()
	return s.sessions.Create(ctx, &turn.Session{
		ID: sess.ID, OwnerID: sess.OwnerID, AgentID: sess.PersonaID,
		Metadata: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now,
	})
}

type httpHandoffReplies struct {
	replies handoff.ReplyStore
}

func (r *httpHandoffReplies) List(ctx context.Context, sessionID string, afterSequence uint64, limit int) ([]httpchat.HandoffReply, error) {
	stored, err := r.replies.List(ctx, sessionID, afterSequence, limit)
	if err != nil {
		return nil, err
	}
	replies := make([]httpchat.HandoffReply, 0, len(stored))
	for i := range stored {
		replies = append(replies, httpchat.HandoffReply{Sequence: stored[i].Sequence, Text: stored[i].Text, CreatedAt: stored[i].CreatedAt})
	}
	return replies, nil
}

func buildHTTPChat(cfg *appconfig.Config, db *sql.DB, backend turn.AgentRuntime, registry *auraagent.Registry, recorder *telemetry.AccessRecorder, approvals *approval.OwnerRoute, logger *slog.Logger) (*httpchat.Server, []health.RegisteredCheck, error) {
	settings := &cfg.Channels.HTTP
	if !settings.Enabled {
		return nil, nil, nil
	}
	replyStore, err := handoff.NewReplyStore(turnstore.NewTurnStore(db))
	if err != nil {
		return nil, nil, err
	}
	srv, err := httpchat.New(&httpchat.Deps{
		Config:        settings,
		Keys:          &apiKeyDirectory{keys: principal.NewAPIKeyStore(db)},
		Sessions:      &httpSessionStore{sessions: turnstore.NewSessionService(db)},
		Replies:       &httpHandoffReplies{replies: replyStore},
		Personas:      registry,
		Runtime:       backend,
		Access:        channelAccessSettings(cfg, db, recorder, nil),
		Approvals:     approvals,
		AnswerTexts:   runtimeengine.DeliveryTexts,
		ShutdownGrace: time.Duration(cfg.Runtime.ShutdownTimeout) / httpChatDrainDivisor,
		Logger:        logger,
	})
	if err != nil {
		return nil, nil, err
	}
	checks := srv.HealthChecks()
	for i := range checks {
		if checks[i].Timeout == 0 {
			checks[i].Timeout = time.Duration(cfg.Health.CheckTimeout)
		}
	}
	return srv, checks, nil
}

type discordBroadcastSender struct {
	adapter  *discord.Adapter
	effects  *effect.Executor
	sessions turnstore.SessionService
}

func (s *discordBroadcastSender) Send(ctx context.Context, req *broadcast.SendRequest) (broadcast.SendOutcome, error) {
	if req == nil {
		return broadcast.SendOutcome{}, errors.New("broadcast send request must not be nil")
	}
	if !isBroadcastChannelID(req.Route.Instance) {
		return broadcast.SendOutcome{}, nil
	}
	if s.adapter == nil || s.effects == nil {
		return broadcast.SendOutcome{}, errors.New("discord broadcast sender is not wired")
	}
	if s.sessions != nil {
		if err := ensureBroadcastSession(ctx, s.sessions, req.SessionID); err != nil {
			return broadcast.SendOutcome{}, err
		}
	}
	payload, err := json.Marshal(discord.SendOperation{ChannelID: req.Route.Instance, Text: req.Text, Previews: !req.Tainted})
	if err != nil {
		return broadcast.SendOutcome{}, err
	}
	intent, err := s.effects.Execute(ctx, &effect.PrepareRequest{
		SessionID:      req.SessionID,
		IdempotencyKey: req.IdempotencyKey,
		Provider:       "discord",
		Operation:      discord.OpSendMessage,
		Classification: effect.ClassificationEffectful,
		Request:        payload,
		EventKind:      effect.EventKindChannelRequested,
	}, s.adapter)
	if err != nil {
		return broadcast.SendOutcome{}, err
	}
	switch intent.State {
	case effect.StateSucceeded:
		return broadcast.SendOutcome{IntentID: intent.ID, Receipt: intent.ProviderReceipt, Succeeded: true}, nil
	case effect.StateUnknown, effect.StateStarted:
		return broadcast.SendOutcome{IntentID: intent.ID, Ambiguous: true}, nil
	default:
		return broadcast.SendOutcome{IntentID: intent.ID}, nil
	}
}

func isBroadcastChannelID(id string) bool {
	if id == "" {
		return false
	}
	for i := range len(id) {
		if id[i] < '0' || id[i] > '9' {
			return false
		}
	}
	return true
}

func ensureBroadcastSession(ctx context.Context, sessions turnstore.SessionService, sessionID string) error {
	if sessions == nil {
		return nil
	}
	if _, err := sessions.Get(ctx, sessionID); err == nil {
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		if code, ok := fault.CodeOf[turnstore.ErrorCode](err); !ok || code != turnstore.ErrorCodeSessionNotFound {
			return err
		}
	}
	now := time.Now().UTC()
	err := sessions.Create(ctx, &turn.Session{
		ID:        sessionID,
		OwnerID:   "broadcast",
		Metadata:  json.RawMessage(`{}`),
		CreatedAt: now,
		UpdatedAt: now,
	})
	if code, ok := fault.CodeOf[turnstore.ErrorCode](err); ok && code == turnstore.ErrorCodeSessionIDConflict {
		return nil
	}
	return err
}

func buildBroadcastSenders(adapters []runtimechannelhost.ChannelPort, effects *effect.Executor, sessions turnstore.SessionService) map[string]broadcast.Sender {
	senders := map[string]broadcast.Sender{}
	for _, adapter := range adapters {
		concrete, ok := adapter.(*discord.Adapter)
		if !ok {
			continue
		}
		senders["discord"] = &discordBroadcastSender{adapter: concrete, effects: effects, sessions: sessions}
		break
	}
	return senders
}

type broadcastEffectReconciler struct {
	executor *effect.Executor
}

func (r *broadcastEffectReconciler) GetIntent(ctx context.Context, id string) (*effect.Intent, error) {
	return r.executor.Journal().Get(ctx, id)
}

func (r *broadcastEffectReconciler) Reconcile(ctx context.Context, id string, provider effect.Provider) (*effect.Intent, error) {
	return r.executor.Reconcile(ctx, id, provider, nil)
}

func broadcastEffectProviders(cfg *appconfig.Config, db *sql.DB, logger *slog.Logger) (map[string]effect.Provider, error) {
	providers := map[string]effect.Provider{}
	if cfg == nil || !cfg.Channels.Discord.Enabled || len(cfg.Channels.Discord.Accounts) == 0 {
		return providers, nil
	}
	_, artifactRoot, _, err := storage.ResolvePaths(&cfg.Storage)
	if err != nil {
		return nil, err
	}
	adapter, err := buildDiscordAdapter(cfg, &cfg.Channels.Discord.Accounts[0], db, artifactRoot, &discord.Deps{Logger: logger})
	if err != nil {
		return nil, err
	}
	providers["discord"] = adapter
	return providers, nil
}

func broadcastRecorderObserver(recorder *telemetry.BroadcastRecorder) broadcast.Observer {
	if recorder == nil {
		return nil
	}
	return func(ctx context.Context, observation *broadcast.Observation) {
		recorder.Record(ctx, &telemetry.BroadcastObservation{
			Priority:    observation.Priority,
			State:       observation.State,
			Result:      observation.Result,
			Attempts:    observation.Attempts,
			Age:         observation.Age,
			RateDelay:   observation.RateDelay,
			DigestCount: observation.DigestCount,
		})
	}
}

type sessionActivity interface {
	CancelActive(ctx context.Context, sessionID string) (turnID string, cancelled bool, err error)
	CancelTurn(ctx context.Context, sessionID, turnID string) (bool, error)
	SessionActivity(sessionID string) (activeTurnID string, queued int)
}

type deferredCommandTurns struct {
	engine atomic.Pointer[sessionActivity]
}

func (t *deferredCommandTurns) bind(engine sessionActivity) {
	if t != nil && engine != nil {
		t.engine.Store(&engine)
	}
}

func (t *deferredCommandTurns) CancelActive(ctx context.Context, sessionID string) (turnID string, cancelled bool, err error) {
	var engine *sessionActivity
	if t != nil {
		engine = t.engine.Load()
	}
	if engine == nil {
		return "", false, command.Errorf(command.ErrorCodeUnavailable, "turn engine is not ready")
	}
	return (*engine).CancelActive(ctx, sessionID)
}

func (t *deferredCommandTurns) CancelTurn(ctx context.Context, sessionID, turnID string) (bool, error) {
	var engine *sessionActivity
	if t != nil {
		engine = t.engine.Load()
	}
	if engine == nil {
		return false, command.Errorf(command.ErrorCodeUnavailable, "turn engine is not ready")
	}
	return (*engine).CancelTurn(ctx, sessionID, turnID)
}

func (t *deferredCommandTurns) Activity(sessionID string) (activeTurnID string, queued int) {
	var engine *sessionActivity
	if t != nil {
		engine = t.engine.Load()
	}
	if engine == nil {
		return "", 0
	}
	return (*engine).SessionActivity(sessionID)
}

type deferredCommandOperations struct {
	chat atomic.Pointer[command.Operations]
}

func (o *deferredCommandOperations) bind(chat command.Operations) error {
	if o == nil || chat == nil {
		return invalidOptions("the chat operations need a projection")
	}
	if !o.chat.CompareAndSwap(nil, &chat) {
		return invalidOptions("the chat operations are already bound")
	}
	return nil
}

func (o *deferredCommandOperations) Plan(group string, tokens []string) command.OperationPlan {
	var chat *command.Operations
	if o != nil {
		chat = o.chat.Load()
	}
	if chat == nil {
		refusal := command.Result{Lines: []string{command.ReplyOperationsUnavailable}, Diagnostic: true, Code: command.ErrorCodeUnavailable}
		return command.OperationPlan{Refusal: &refusal}
	}
	return (*chat).Plan(group, tokens)
}

func (o *deferredCommandOperations) Run(ctx context.Context, run *command.OperationRun) command.OperationReply {
	var chat *command.Operations
	if o != nil {
		chat = o.chat.Load()
	}
	if chat == nil {
		return command.OperationReply{Lines: []string{command.ReplyOperationsUnavailable}, Code: command.ErrorCodeUnavailable}
	}
	return (*chat).Run(ctx, run)
}

var errCommandSkillsUnavailable = errors.New("skill support is not available")

type deferredCommandSkills struct {
	skills atomic.Pointer[command.Skills]
}

func (s *deferredCommandSkills) bind(sk command.Skills) {
	if s != nil && sk != nil {
		s.skills.Store(&sk)
	}
}

func (s *deferredCommandSkills) Activate(ctx context.Context, name string) (command.SkillPin, error) {
	var sk *command.Skills
	if s != nil {
		sk = s.skills.Load()
	}
	if sk == nil {
		return command.SkillPin{}, errCommandSkillsUnavailable
	}
	return (*sk).Activate(ctx, name)
}

func (s *deferredCommandSkills) StageDraft(ctx context.Context, name, body string) (command.Draft, error) {
	var sk *command.Skills
	if s != nil {
		sk = s.skills.Load()
	}
	if sk == nil {
		return command.Draft{}, errCommandSkillsUnavailable
	}
	return (*sk).StageDraft(ctx, name, body)
}

type sequenceReader interface {
	LastSequence(ctx context.Context, sessionID string) (uint64, error)
}

type discordCommandSessions struct {
	sessions turnstore.SessionService
	events   sequenceReader
	pointers turnstore.ConversationSessionStore
	now      func() time.Time
}

func (s *discordCommandSessions) EventCount(ctx context.Context, sessionID string) (int, error) {
	last, err := s.events.LastSequence(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	if last > math.MaxInt32 {
		return math.MaxInt32, nil
	}
	return int(last), nil
}

func (s *discordCommandSessions) Create(ctx context.Context, caller *command.Caller) (command.Session, error) {
	if caller.ConversationKey == "" {
		return command.Session{}, errors.New("conversation key must not be empty")
	}
	suffix, err := conversationSessionSuffix()
	if err != nil {
		return command.Session{}, err
	}
	now := s.clock()
	sess := turn.Session{
		ID:        caller.ConversationKey + conversationSessionSeparator + suffix,
		OwnerID:   caller.PrincipalID,
		AgentID:   caller.AgentID,
		Metadata:  json.RawMessage(`{}`),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.sessions.Create(ctx, &sess); err != nil {
		return command.Session{}, err
	}
	if err := s.pointers.Point(ctx, caller.ConversationKey, sess.ID); err != nil {
		return command.Session{}, err
	}
	return command.Session{ID: sess.ID, OwnerID: sess.OwnerID, AgentID: sess.AgentID}, nil
}

func (s *discordCommandSessions) Switch(ctx context.Context, caller *command.Caller, id string) (command.Session, error) {
	key := caller.ConversationKey
	if key == "" || (id != key && !strings.HasPrefix(id, key+conversationSessionSeparator)) {
		return command.Session{}, command.Errorf(command.ErrorCodeNotSwitchable, "outside this conversation")
	}
	if id == key {
		if err := s.pointers.Clear(ctx, key); err != nil {
			return command.Session{}, err
		}
		return command.Session{ID: key, OwnerID: caller.PrincipalID, AgentID: caller.AgentID}, nil
	}
	sess, err := s.sessions.Get(ctx, id)
	if err != nil {
		if missingSession(err) {
			return command.Session{}, command.Errorf(command.ErrorCodeNotSwitchable, "not found")
		}
		return command.Session{}, err
	}
	if sess.OwnerID != caller.PrincipalID || sess.AgentID != caller.AgentID {
		return command.Session{}, command.Errorf(command.ErrorCodeNotSwitchable, "owned by another principal or persona")
	}
	err = s.pointers.Point(ctx, key, id)
	if err != nil {
		if missingSession(err) {
			return command.Session{}, command.Errorf(command.ErrorCodeNotSwitchable, "not found")
		}
		return command.Session{}, err
	}
	return command.Session{ID: sess.ID, OwnerID: sess.OwnerID, AgentID: sess.AgentID}, nil
}

func (s *discordCommandSessions) clock() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

func conversationSessionSuffix() (string, error) {
	var raw [10]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("conversation session suffix: %w", err)
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:])), nil
}

func missingSession(err error) bool {
	code, ok := fault.CodeOf[turnstore.ErrorCode](err)
	return errors.Is(err, sql.ErrNoRows) || (ok && code == turnstore.ErrorCodeSessionNotFound)
}

type chatCommandDeps struct {
	sessions   turnstore.SessionService
	events     sequenceReader
	pointers   turnstore.ConversationSessionStore
	turns      command.Turns
	skills     command.Skills
	operations command.Operations
	recorder   command.Recorder
	catalogs   []operation.Catalog
}

func addOperationGroups(binding *command.Binding, groups []command.Definition) {
	for i := range groups {
		binding.Names = append(binding.Names, groups[i].Name)
	}
}

func newChatCommandDispatcher(deps *chatCommandDeps) (*command.Dispatcher, error) {
	if deps == nil {
		return nil, invalidOptions("the chat command dispatcher needs its ports")
	}
	groups := operation.ChatGroups(deps.catalogs)
	discordBinding, telegramBinding := discord.CommandBinding(), telegram.CommandBinding()
	addOperationGroups(&discordBinding, groups)
	addOperationGroups(&telegramBinding, groups)
	registry, err := command.NewRegistry(append(command.Catalog(), groups...), discordBinding, telegramBinding)
	if err != nil {
		return nil, err
	}
	return command.NewDispatcher(registry, command.Ports{
		Sessions:   &discordCommandSessions{sessions: deps.sessions, events: deps.events, pointers: deps.pointers},
		Turns:      deps.turns,
		Skills:     deps.skills,
		Operations: deps.operations,
	}, deps.recorder)
}

func githubConfig(cfg *appconfig.Config) *github.Config {
	if cfg == nil || cfg.Tools == nil {
		return nil
	}
	return &cfg.Tools.GitHub
}

type workflowCorrelationStore struct {
	store *workflow.Store
}

func newWorkflowCorrelationStore(store *workflow.Store) *workflowCorrelationStore {
	return &workflowCorrelationStore{store: store}
}

func (s *workflowCorrelationStore) BindCorrelation(ctx context.Context, correlation *github.Correlation) error {
	if correlation == nil {
		return fault.New(github.ErrorCodeInvalidArgument, "correlation must not be nil")
	}
	err := s.store.BindCorrelation(ctx, &workflow.Correlation{
		Source:     correlation.Source,
		EventType:  correlation.EventType,
		ExternalID: correlation.ExternalID,
		RunID:      correlation.RunID,
		SignalName: correlation.SignalName,
		DedupeKey:  correlation.DedupeKey,
	})
	return mapCorrelationError(err)
}

func (s *workflowCorrelationStore) ResolveCorrelation(ctx context.Context, source, eventType, externalID, dedupeKey string) (github.Correlation, error) {
	found, err := s.store.ResolveCorrelation(ctx, source, eventType, externalID, dedupeKey)
	if err != nil {
		return github.Correlation{}, mapCorrelationError(err)
	}
	return github.Correlation{
		Source:     found.Source,
		EventType:  found.EventType,
		ExternalID: found.ExternalID,
		RunID:      found.RunID,
		SignalName: found.SignalName,
		DedupeKey:  found.DedupeKey,
	}, nil
}

func (s *workflowCorrelationStore) DeleteCorrelation(ctx context.Context, source, eventType, externalID, dedupeKey string) error {
	return mapCorrelationError(s.store.DeleteCorrelation(ctx, source, eventType, externalID, dedupeKey))
}

func (s *workflowCorrelationStore) RunDurableKey(ctx context.Context, runID string) (string, error) {
	summary, err := s.store.Run(ctx, runID)
	if err != nil {
		return "", err
	}
	return summary.DurableKey, nil
}

func mapCorrelationError(err error) error {
	if err == nil {
		return nil
	}
	if code, ok := fault.CodeOf[workflow.ErrorCode](err); ok {
		if code == workflow.ErrorCodeCorrelationUnmatched {
			return fault.Wrap(github.ErrorCodeCorrelationUnmatched, err.Error(), err)
		}
		if code == workflow.ErrorCodeCorrelationConflict {
			return fault.Wrap(github.ErrorCodeCorrelationConflict, err.Error(), err)
		}
	}
	return err
}

type channelHandoffTarget interface {
	NotifyHandoff(ctx context.Context, sessionID, reason string) (access.HandoffTarget, error)
}

type channelHandoffNotifier struct {
	target channelHandoffTarget
}

func (n *channelHandoffNotifier) NotifyHandoff(ctx context.Context, sessionID, reason string) (handoff.Target, error) {
	target, err := n.target.NotifyHandoff(ctx, sessionID, reason)
	if errors.Is(err, access.ErrNoHandoffTarget) {
		return handoff.Target{}, handoff.ErrNoTarget
	}
	if err != nil {
		return handoff.Target{}, err
	}
	return handoff.Target{
		Channel:   target.Channel,
		Account:   target.Account,
		ChannelID: target.ChannelID,
		ThreadID:  target.ThreadID,
	}, nil
}

func handoffNotifiers(ports []runtimechannelhost.ChannelPort) []handoff.Notifier {
	var notifiers []handoff.Notifier
	for _, port := range ports {
		if target, ok := port.(channelHandoffTarget); ok {
			notifiers = append(notifiers, &channelHandoffNotifier{target: target})
		}
	}
	return notifiers
}

type ownerHandoffNotifier interface {
	approval.PersonaServer
	NotifyOwnerHandoff(ctx context.Context, requester *approval.Requester, sessionID, reason string) error
}

type ownerHandoffAdapter struct {
	notifier ownerHandoffNotifier
}

func (a *ownerHandoffAdapter) ServesPersona(persona string) bool {
	return a.notifier.ServesPersona(persona)
}

func (a *ownerHandoffAdapter) NotifyOwnerHandoff(ctx context.Context, requester *handoff.OwnerRequester, sessionID, reason string) error {
	return a.notifier.NotifyOwnerHandoff(ctx, &approval.Requester{
		Principal: requester.Principal,
		Class:     requester.Class,
		Persona:   requester.Persona,
	}, sessionID, reason)
}

type emailOwnerHandoffRouter struct {
	router *handoff.OwnerRouter
}

func (r *emailOwnerHandoffRouter) NotifyOwner(ctx context.Context, requester *approval.Requester, sessionID, reason string) error {
	return r.router.NotifyOwner(ctx, &handoff.OwnerRequester{
		Principal: requester.Principal,
		Class:     requester.Class,
		Persona:   requester.Persona,
	}, sessionID, reason)
}

func newOwnerHandoffRouter(notifiers []ownerHandoffNotifier) *handoff.OwnerRouter {
	adapted := make([]handoff.OwnerNotifier, 0, len(notifiers))
	for _, notifier := range notifiers {
		adapted = append(adapted, &ownerHandoffAdapter{notifier: notifier})
	}
	return handoff.NewOwnerRouter(adapted)
}

type httpRequesters interface {
	RequesterOf(sessionID string) (approval.Requester, bool)
}

type httpRequesterLookupAdapter struct {
	requesters httpRequesters
}

func (a *httpRequesterLookupAdapter) RequesterOf(sessionID string) (handoff.OwnerRequester, bool) {
	req, ok := a.requesters.RequesterOf(sessionID)
	if !ok {
		return handoff.OwnerRequester{}, false
	}
	return handoff.OwnerRequester{Principal: req.Principal, Class: req.Class, Persona: req.Persona}, true
}

func newHTTPHandoffNotifier(requesters httpRequesters, adapters []approvalAdapter) (*handoff.HTTPNotifier, error) {
	notifiers := make([]ownerHandoffNotifier, 0, len(adapters))
	for _, adapter := range adapters {
		notifiers = append(notifiers, adapter)
	}
	return handoff.NewHTTPNotifier(&httpRequesterLookupAdapter{requesters: requesters}, newOwnerHandoffRouter(notifiers))
}

func discordAccount(cfg *appconfig.Config, name string) (*discord.Account, bool) {
	for i := range cfg.Channels.Discord.Accounts {
		if cfg.Channels.Discord.Accounts[i].Name == name {
			return &cfg.Channels.Discord.Accounts[i], true
		}
	}
	return nil, false
}

func telegramAccount(cfg *appconfig.Config, name string) (*telegram.Account, bool) {
	for i := range cfg.Channels.Telegram.Accounts {
		if cfg.Channels.Telegram.Accounts[i].Name == name {
			return &cfg.Channels.Telegram.Accounts[i], true
		}
	}
	return nil, false
}

func emailAccount(cfg *appconfig.Config, name string) (*email.Account, bool) {
	for i := range cfg.Channels.Email.Accounts {
		if cfg.Channels.Email.Accounts[i].Name == name {
			return &cfg.Channels.Email.Accounts[i], true
		}
	}
	return nil, false
}

func deliverToHandoff(ctx context.Context, cfg *appconfig.Config, h *handoff.Handoff, text string) error {
	switch h.Channel {
	case handoff.ChannelTelegram:
		account, ok := telegramAccount(cfg, h.Account)
		if !ok {
			return clikit.NewExitCodeError(1, fmt.Errorf("handoff account %q is not configured", h.Account))
		}
		return deliverTelegramText(ctx, account, h.ReplyChannelID, h.ReplyThreadID, text)
	case handoff.ChannelHTTP:
		return nil
	case handoff.ChannelEmail:
		account, ok := emailAccount(cfg, h.Account)
		if !ok {
			return clikit.NewExitCodeError(1, fmt.Errorf("handoff account %q is not configured", h.Account))
		}
		return deliverEmailText(ctx, account, &email.TextReply{To: h.ReplyChannelID, Thread: email.ParseThreadReference(h.ReplyThreadID), Text: text})
	case handoff.ChannelDiscord:
		account, ok := discordAccount(cfg, h.Account)
		if !ok {
			return clikit.NewExitCodeError(1, fmt.Errorf("handoff account %q is not configured", h.Account))
		}
		return deliverText(ctx, account, h.ReplyChannelID, text)
	default:
		return clikit.NewExitCodeError(1, fmt.Errorf("handoff channel %q is not supported", h.Channel))
	}
}

func appendPersonaReply(ctx context.Context, db *sql.DB, h *handoff.Handoff, author, text string, at time.Time) error {
	return appendSessionText(ctx, db, h.SessionID, handoff.ReplyEventID(at), author, text, at)
}

func sessionEventWithText(id, author, text string, at time.Time) *session.Event {
	event := &session.Event{ID: id, InvocationID: id, Author: author, Timestamp: at}
	event.Content = genai.NewContentFromText(text, genai.RoleModel)
	return event
}

func appendSessionText(ctx context.Context, db *sql.DB, sessionID, id, author, text string, at time.Time) error {
	event, err := turnstore.RuntimeEventFromADK(sessionID, id, sessionEventWithText(id, author, text, at))
	if err != nil {
		return err
	}
	_, err = turnstore.NewEventStore(db).AppendSequenced(ctx, sessionID, &event)
	return err
}

func codeObserver(recorder *telemetry.CodeRecorder) codeexec.Observer {
	return func(ctx context.Context, observation *codeexec.Observation) {
		recorder.Record(ctx, &telemetry.CodeObservation{
			Runtime: observation.Runtime, ResultClass: string(observation.Class), Phase: string(observation.Phase),
			Compiled: observation.Compiled, BuildDuration: observation.Build.Duration, RunDuration: observation.Run.Duration,
			BuildCPU: observation.Build.CPUTime, RunCPU: observation.Run.CPUTime,
			OutputBytes: observation.StdoutBytes + observation.StderrBytes, ArtifactBytes: observation.ArtifactBytes,
		})
	}
}

func interpreterEnabled(report capability.Report) bool {
	status, ok := report.Status(codeexec.CapabilityName)
	return ok && status.Enabled && status.Available
}

func interpreterStatusChecks(cfg *appconfig.Config, report capability.Report) ([]health.RegisteredCheck, error) {
	if !interpreterEnabled(report) {
		return nil, nil
	}
	dataRoot, err := storage.ResolveDataRoot(&cfg.Storage)
	if err != nil {
		return nil, err
	}
	return codeexec.HealthChecks(dataRoot, nil, codeexec.DefaultVerifyTimeout)
}

func newKnowledgeService(cfg *appconfig.Config, db *sql.DB, resolver egress.Resolver) (*knowledge.Service, error) {
	if cfg == nil {
		return nil, errors.New("knowledge config must not be nil")
	}
	if db == nil {
		return nil, errors.New("knowledge database must not be nil")
	}
	kbStore, err := knowledge.NewSQLiteStore(db)
	if err != nil {
		return nil, err
	}
	wiring, err := knowledge.NewEmbeddingWiring(cfg.Knowledge, resolver)
	if err != nil {
		return nil, err
	}
	return knowledge.NewService(&knowledge.Deps{
		Store:     kbStore,
		Secrets:   &knowledgeSecrets{cfg: cfg},
		Reducer:   fetch.HTMLToMarkdown,
		HTTP:      knowledge.NewHTTPClient(resolver),
		Actions:   &knowledgeActions{db: db},
		Vectors:   kbStore,
		Embedder:  wiring.Embedder,
		Embedding: wiring.Settings,
	})
}

type knowledgeSecrets struct {
	cfg *appconfig.Config
}

func (s *knowledgeSecrets) Snapshot(_ context.Context) (knowledge.SecretScreen, error) {
	vault, err := appconfig.BuildStrictSecretVault(s.cfg)
	if err != nil {
		return nil, err
	}
	return vault, nil
}

type knowledgeActions struct {
	db *sql.DB
}

func (a *knowledgeActions) RecordOwnerAction(ctx context.Context, action *knowledge.OwnerAction) error {
	if action == nil {
		return errors.New("cli: knowledge owner action must not be nil")
	}
	sessionID, err := turnstore.EnsureOwnerActionSession(ctx, a.db, principal.OwnerID)
	if err != nil {
		return err
	}
	at := action.At.UTC().Format(time.RFC3339Nano)
	payload, err := json.Marshal(struct {
		Action     string `json:"action"`
		Collection string `json:"collection"`
		SourceID   string `json:"source_id"`
		Kind       string `json:"kind,omitempty"`
		Outcome    string `json:"outcome"`
		Chunks     int    `json:"chunks"`
		At         string `json:"at"`
	}{
		Action: action.Action, Collection: action.Collection, SourceID: action.SourceID,
		Kind: action.Kind, Outcome: action.Outcome, Chunks: action.Chunks, At: at,
	})
	if err != nil {
		return fmt.Errorf("cli: marshal knowledge owner action: %w", err)
	}
	_, _, err = turnstore.NewEventStore(a.db).UpsertEvent(ctx, &turn.Event{
		ID:            "kb-owner-action-" + action.Action + "-" + action.SourceID + "-" + at,
		SessionID:     sessionID,
		TurnID:        "owner-action",
		InvocationID:  "owner-action",
		Author:        principal.OwnerID,
		Kind:          knowledgeOwnerActionKind,
		SchemaVersion: 1,
		Payload:       payload,
		CreatedAt:     action.At.UTC(),
	})
	return err
}

func personaKnowledgeScopes(agents *auraagent.Config) []knowledge.PersonaScope {
	var scopes []knowledge.PersonaScope
	if agents == nil {
		return nil
	}
	for i := range agents.Definitions {
		definition := &agents.Definitions[i]
		for _, list := range []struct {
			class       string
			collections []string
		}{
			{"owner", definition.Knowledge},
			{"trusted", definition.PrincipalKnowledge},
			{"public", definition.PublicKnowledge},
		} {
			if len(list.collections) > 0 {
				scopes = append(scopes, knowledge.PersonaScope{
					Persona:     definition.ID,
					Class:       list.class,
					Collections: list.collections,
				})
			}
		}
	}
	return scopes
}

func knowledgeStatusChecks(cfg *appconfig.Config) ([]health.RegisteredCheck, error) {
	if cfg == nil {
		return nil, nil
	}
	dbPath, _, _, err := storage.ResolvePaths(&cfg.Storage)
	if err != nil {
		return nil, err
	}
	return knowledge.HealthChecks(cfg.Knowledge, personaKnowledgeScopes(cfg.Agents), dbPath, time.Duration(cfg.Health.CheckTimeout)), nil
}

type discardRedactor struct{}

func (discardRedactor) RegisterSecret(string) {}

type pluginAuditAdapter struct {
	db *sql.DB
}

func (a *pluginAuditAdapter) Record(ctx context.Context, action, pluginID, ver string) error {
	subject := pluginID
	if action == plugin.AuditActionApprove && ver != "" {
		subject = pluginID + "-" + ver
	}
	return turnstore.NewOwnerActionRecorder(a.db).RecordAccessAction(ctx, &turnstore.AccessAction{
		Action:  action,
		Plugin:  pluginID,
		Version: ver,
	}, subject, time.Now())
}

func openPluginService(cfg *appconfig.Config, db *sql.DB, redactor auth.Redactor) (*plugin.Service, error) {
	dataRoot, err := storage.ResolveDataRoot(&cfg.Storage)
	if err != nil {
		return nil, err
	}
	return plugin.OpenService(db, dataRoot, redactor, nil)
}

func buildPluginModule(ctx context.Context, cfg *appconfig.Config, db *sql.DB, broker *toolbroker.Broker, capabilities capability.Report, logger *slog.Logger) (*plugin.Module, error) {
	dataRoot, err := storage.ResolveDataRoot(&cfg.Storage)
	if err != nil {
		return nil, err
	}
	return plugin.New(ctx, &plugin.ModuleDeps{
		Config:       cfg.Plugins,
		DB:           db,
		DataRoot:     dataRoot,
		Broker:       broker,
		Capabilities: capabilities,
		Validator:    mcp.MakeValidator,
		Logger:       logger,
	})
}

func mcpServerFromBackend(s *plugin.BackendServer) mcp.ServerConfig {
	out := mcp.ServerConfig{
		Name:           s.Name,
		Transport:      s.Transport,
		Command:        s.Command,
		Args:           slices.Clone(s.Args),
		URL:            s.URL,
		StartupTimeout: s.StartupTimeout,
		RequestTimeout: s.RequestTimeout,
		MaxMessageSize: s.MaxMessageSize,
		Capabilities:   slices.Clone(s.Capabilities),
	}
	if s.ConnectTimeout > 0 {
		ct := s.ConnectTimeout
		out.ConnectTimeout = &ct
	}
	return out
}

func checkMCPWorkingDir(cfg *appconfig.Config, plugins []mcp.PluginServer) error {
	if !runsStdioServer(cfg, plugins) {
		return nil
	}
	workingDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve the working directory: %w", err)
	}
	dataRoot, err := storage.ResolveDataRoot(&cfg.Storage)
	if err != nil {
		return err
	}
	if !appconfig.ContainsPath(workingDir, dataRoot) {
		return nil
	}
	return configkit.Invalid("mcp", fmt.Sprintf("a stdio server gets the working directory %s read-write, which holds the data directory %s; start the daemon from another directory", workingDir, dataRoot))
}

func runsStdioServer(cfg *appconfig.Config, plugins []mcp.PluginServer) bool {
	servers := configuredMCPServers(cfg)
	for i := range servers {
		if servers[i].Transport == mcp.TransportStdio {
			return true
		}
	}
	for i := range plugins {
		if plugins[i].Server.Transport == mcp.TransportStdio {
			return true
		}
	}
	return false
}

func mcpPluginsFromBackends(backends []plugin.Active) []mcp.PluginServer {
	servers := make([]mcp.PluginServer, 0, len(backends))
	for i := range backends {
		active := &backends[i]
		if active.Kind != plugin.BackendMCP {
			continue
		}
		srv := mcp.PluginServer{
			ID:         active.ID,
			Server:     mcpServerFromBackend(&active.Server),
			Authorizer: active.Auth,
		}
		for _, tool := range active.Tools {
			srv.Tools = append(srv.Tools, mcp.PluginTool{
				Name:        tool.Name,
				Description: tool.Description,
				Display:     slices.Clone(tool.Display),
			})
		}
		servers = append(servers, srv)
	}
	return servers
}

func mcpPluginStatuses(mcpServers *mcp.Module) []plugin.RuntimeStatus {
	if mcpServers == nil {
		return nil
	}
	statuses := mcpServers.PluginStatuses()
	out := make([]plugin.RuntimeStatus, 0, len(statuses))
	for _, status := range statuses {
		out = append(out, plugin.RuntimeStatus{
			ID:      status.ID,
			Running: status.Code == "" && status.Connected,
			Drift:   status.Drift,
		})
	}
	return out
}

func pluginHealthCheck(mod *plugin.Module, mcpServers *mcp.Module, timeout time.Duration) (health.RegisteredCheck, bool, error) {
	if mod == nil {
		return health.RegisteredCheck{}, false, nil
	}
	var mcpStatus func() []plugin.RuntimeStatus
	if mcpServers != nil {
		mcpStatus = func() []plugin.RuntimeStatus {
			return mcpPluginStatuses(mcpServers)
		}
	}
	return mod.HealthCheck(mcpStatus, timeout)
}

type cronTurnEngine interface {
	Run(ctx context.Context, req *turn.TurnRequest) iter.Seq2[turn.Event, error]
}

type workflowStarter interface {
	StartConfigured(ctx context.Context, start *workflow.ConfiguredStart) (*workflow.RunSummary, error)
}

type scheduleWorkflows struct {
	starter atomic.Pointer[workflowStarter]
}

func (w *scheduleWorkflows) set(starter workflowStarter) {
	if w != nil && starter != nil {
		w.starter.Store(&starter)
	}
}

func (w *scheduleWorkflows) start(ctx context.Context, req *schedule.TurnRequest) (schedule.TurnResult, error) {
	var starter *workflowStarter
	if w != nil {
		starter = w.starter.Load()
	}
	if starter == nil {
		return schedule.TurnResult{}, errors.New("scheduled workflows have no workflow interpreter")
	}
	input := &workflow.RunInput{}
	if req.WorkflowInput != "" {
		if err := json.Unmarshal([]byte(req.WorkflowInput), input); err != nil {
			return schedule.TurnResult{}, fmt.Errorf("scheduled workflow input: %w", err)
		}
	}
	run, err := (*starter).StartConfigured(ctx, &workflow.ConfiguredStart{
		DefinitionID: req.WorkflowID,
		Input:        &workflow.RunInput{Objective: input.Objective},
		PrincipalID:  req.PrincipalID,
		Class:        principal.ClassForPrincipal(req.PrincipalID),
		DurableKey:   req.IdempotencyKey,
		Scheduled:    true,
		Tainted:      req.TaintSource != "",
	})
	if fault.HasCode(err, workflow.ErrorCodeUnattendedSink) {
		return schedule.TurnResult{
			SafeErrorCode: string(workflow.ErrorCodeUnattendedSink),
			Text:          fmt.Sprintf("Scheduled workflow %s did not start: a step after a web step could send data out. Remove that step or move it before the web step.", req.WorkflowID),
			Tainted:       req.TaintSource != "",
		}, nil
	}
	if err != nil {
		return schedule.TurnResult{}, err
	}
	return schedule.TurnResult{TurnID: run.ID, Text: fmt.Sprintf("started workflow %s (run %s)", req.WorkflowID, run.ID), Succeeded: true, Tainted: req.TaintSource != ""}, nil
}

type engineTurnRunner struct {
	engine    cronTurnEngine
	sessions  turnstore.SessionService
	workflows *scheduleWorkflows
	webTools  schedule.WebToolsOffered
	personas  *auraagent.Registry
}

const webOnlyUnavailableCode = string(turn.ErrorCodeWebOnlyUnavailable)

func webToolsOfPersona(load func() (*auraagent.Registry, error)) schedule.WebToolsOffered {
	return func(context.Context) (bool, error) {
		registry, err := load()
		if err != nil {
			return false, err
		}
		definition, err := registry.Resolve(nil, nil)
		if err != nil {
			return false, err
		}
		for _, name := range turn.WebOnlyRestriction().Allowed {
			if definition.OffersPlainTool(principal.ClassOwner, name) {
				return true, nil
			}
		}
		return false, nil
	}
}

func (r *engineTurnRunner) webOnlyRefusal(ctx context.Context, req *schedule.TurnRequest) (schedule.TurnResult, bool) {
	if req.Context != schedule.ContextWebOnly || r.webTools == nil {
		return schedule.TurnResult{}, false
	}
	offered, err := r.webTools(ctx)
	if err == nil && offered {
		return schedule.TurnResult{}, false
	}
	return schedule.TurnResult{
		SafeErrorCode: webOnlyUnavailableCode,
		Text:          "Scheduled web-only job did not run: the persona that runs scheduled jobs offers neither web_search nor web_fetch. Add one to its tools or change the job to the full context.",
		Tainted:       req.TaintSource != "" || req.Context == schedule.ContextWebOnly,
	}, true
}

func (r *engineTurnRunner) isTainted(turnReq *turn.TurnRequest) bool {
	if turnReq.Tainted() {
		return true
	}
	if turnReq.WebOnly || r.personas == nil {
		return false
	}
	def, err := r.personas.Resolve(nil, nil)
	if err != nil {
		return true
	}
	return len(def.KnowledgeFor(turnReq.PrincipalClass)) > 0
}

func (r *engineTurnRunner) RunTurn(ctx context.Context, req *schedule.TurnRequest) (schedule.TurnResult, error) {
	if req == nil {
		return schedule.TurnResult{}, errors.New("schedule turn request must not be nil")
	}
	if req.WorkflowID != "" {
		return r.workflows.start(ctx, req)
	}
	if r.engine == nil {
		return schedule.TurnResult{}, errors.New("turn engine is not wired")
	}
	if refused, found := r.webOnlyRefusal(ctx, req); found {
		return refused, nil
	}
	if r.sessions != nil {
		if err := ensureCronSession(ctx, r.sessions, req.SessionID); err != nil {
			return schedule.TurnResult{}, err
		}
	}
	turnReq := scheduledTurn(req)
	var stream []terminal.Event
	var turnID string
	for event, err := range r.engine.Run(ctx, turnReq) {
		if err != nil {
			if fault.HasCode(err, turn.ErrorCodeRuntimeOverloaded) {
				return schedule.TurnResult{}, schedule.Errorf(schedule.ErrorCodeRuntimeOverloaded, "runtime is overloaded")
			}
			return schedule.TurnResult{}, err
		}
		if turnID == "" {
			turnID = event.TurnID
		}
		stream = append(stream, terminal.Event{Kind: event.Kind, Author: event.Author, TurnID: event.TurnID, Payload: event.Payload})
	}
	text, _, terminalReached := terminal.PlainRenderer{}.RenderTurn(stream)
	if !terminalReached {
		return schedule.TurnResult{}, errors.New("turn stream ended before a terminal event")
	}
	completed := false
	for _, event := range stream {
		if event.Kind == turn.EventKindTurnCompleted {
			completed = true
		}
	}
	result := schedule.TurnResult{TurnID: turnID, Text: text, Succeeded: completed, Tainted: r.isTainted(turnReq)}
	if !completed {
		result.SafeErrorCode = "turn_failed"
	}
	return result, nil
}

func scheduledTurn(req *schedule.TurnRequest) *turn.TurnRequest {
	return &turn.TurnRequest{
		SessionID:      req.SessionID,
		PrincipalID:    req.PrincipalID,
		PrincipalClass: principal.ClassForPrincipal(req.PrincipalID),
		Origin:         turn.Origin(req.Origin),
		Parts:          []runtimeingress.InputPart{{Text: req.Prompt}},
		IdempotencyKey: req.IdempotencyKey,
		JobID:          req.JobID,
		JobTaint:       req.TaintSource,
		WebOnly:        req.Context == schedule.ContextWebOnly,
		Timezone:       req.Timezone,
	}
}

func ensureCronSession(ctx context.Context, sessions turnstore.SessionService, sessionID string) error {
	if sessions == nil {
		return nil
	}
	if _, err := sessions.Get(ctx, sessionID); err == nil {
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		if code, ok := fault.CodeOf[turnstore.ErrorCode](err); !ok || code != turnstore.ErrorCodeSessionNotFound {
			return err
		}
	}
	now := time.Now().UTC()
	err := sessions.Create(ctx, &turn.Session{
		ID: sessionID, OwnerID: principal.OwnerID,
		Metadata: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now,
	})
	if code, ok := fault.CodeOf[turnstore.ErrorCode](err); ok && code == turnstore.ErrorCodeSessionIDConflict {
		return nil
	}
	return err
}

type cronNotifier struct {
	broadcaster *broadcast.Broadcaster
}

func (n *cronNotifier) Notify(ctx context.Context, req *schedule.NotifyRequest) error {
	if req == nil {
		return errors.New("schedule notify request must not be nil")
	}
	if n.broadcaster == nil {
		return errors.New("broadcaster is not wired")
	}
	_, _, err := n.broadcaster.Submit(ctx, &broadcast.Notification{
		Producer:         req.Producer,
		IdempotencyKey:   req.Key,
		Priority:         req.Priority,
		DestinationAlias: req.DestinationAlias,
		ContentJSON:      notifyContentJSON(req.Text),
		CreatedAt:        time.Now().UTC(),
		Tainted:          req.Tainted,
	})
	return err
}

func notifyContentJSON(text string) string {
	raw, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return `{"text":""}`
	}
	return string(raw)
}

func checkScheduledWorkflow(ctx context.Context, cfg *appconfig.Config, db *sql.DB, id string) error {
	source := &definitionSource{cfg: cfg, db: db}
	spec, err := source.spec(ctx, id)
	if err != nil {
		return err
	}
	deps, err := workflowValidationDeps(cfg)
	if err != nil {
		return err
	}
	lookup := func(child string) (*workflow.Spec, error) { return source.spec(ctx, child) }
	return operation.ModuleFailure[workflow.ErrorCode](workflow.CheckUnattendedSinks(spec, deps, lookup))
}

type definitionSource struct {
	cfg *appconfig.Config
	db  *sql.DB
}

func (s *definitionSource) spec(ctx context.Context, id string) (*workflow.Spec, error) {
	specs, err := workflow.LoadDefinitionsDir(workflowDefinitionsDir(s.cfg))
	if err != nil {
		return nil, err
	}
	for i := range specs {
		if specs[i].ID == id {
			return specs[i], nil
		}
	}
	runs, err := workflow.NewStore(s.db, turnstore.NewTurnStore(s.db))
	if err != nil {
		return nil, err
	}
	spec, err := runs.LatestDefinition(ctx, id)
	if err != nil {
		return nil, operation.NewError(operation.ErrorCodeInputInvalid, fmt.Sprintf("workflow %q is neither a definition nor a saved routine", id))
	}
	return spec, nil
}

func scheduleRecorderObserver(recorder *telemetry.ScheduleRecorder) schedule.Observer {
	if recorder == nil {
		return nil
	}
	return func(ctx context.Context, observation *schedule.Observation) {
		recorder.Record(ctx, &telemetry.ScheduleObservation{
			State:  observation.State,
			Result: observation.Result,
			Lag:    observation.Lag,
			Age:    observation.Age,
		})
	}
}

type stepCatalog struct {
	definitions map[string]*tools.Definition
}

func newStepCatalog(extra []tools.Definition) *stepCatalog {
	builtins := appconfig.BuiltinToolDefinitions()
	catalog := &stepCatalog{definitions: make(map[string]*tools.Definition, len(builtins)+len(extra))}
	for i := range builtins {
		catalog.definitions[builtins[i].Name] = &builtins[i]
	}
	for i := range extra {
		catalog.definitions[extra[i].Name] = &extra[i]
	}
	return catalog
}

func (c *stepCatalog) sinkToolNames() []string {
	var names []string
	for name, definition := range c.definitions {
		if definition.IsExternalSink() {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

func (c *stepCatalog) Lookup(name string) (*tools.Definition, bool) {
	definition, ok := c.definitions[name]
	return definition, ok
}

func (c *stepCatalog) CheckArguments(name string, args json.RawMessage) error {
	definition, ok := c.definitions[name]
	if !ok {
		return toolbroker.Errorf(toolbroker.ResultPolicyDenied, "unknown tool %q version %q", name, tools.Version)
	}
	if _, err := definition.Validate(args); err != nil {
		return toolbroker.Errorf(toolbroker.ResultInvalidArgument, "%v", err)
	}
	return nil
}

func definitionNames() []string {
	builtins := appconfig.BuiltinToolDefinitions()
	names := make([]string, 0, len(builtins))
	for i := range builtins {
		names = append(names, builtins[i].Name)
	}
	slices.Sort(names)
	return names
}

func catalogDefinitions() []runtimeadk.BuiltinToolDefinition {
	builtins := appconfig.BuiltinToolDefinitions()
	out := make([]runtimeadk.BuiltinToolDefinition, 0, len(builtins))
	for i := range builtins {
		out = append(out, runtimeadk.BuiltinToolDefinition{
			Name:                 builtins[i].Name,
			Version:              builtins[i].Version,
			Channel:              builtins[i].Channel,
			Description:          "Aura built-in " + builtins[i].Name + " tool",
			Schema:               builtins[i].Schema,
			RequiredCapabilities: builtins[i].RequiredCapabilities,
			OptIn:                builtins[i].OptIn,
			OwnerOptIn:           builtins[i].OwnerOptIn,
		})
	}
	return out
}

func effectfulToolNames() []string {
	builtins := appconfig.BuiltinToolDefinitions()
	names := make([]string, 0, len(builtins))
	for i := range builtins {
		if builtins[i].RequiresApproval || builtins[i].Effectful {
			names = append(names, builtins[i].Name)
		}
	}
	slices.Sort(names)
	return names
}

func stepToolNames(personaTools []string) []string {
	orchestration := map[string]bool{}
	builtins := appconfig.BuiltinToolDefinitions()
	for i := range builtins {
		if builtins[i].Orchestration {
			orchestration[builtins[i].Name] = true
		}
	}
	return slices.DeleteFunc(slices.Clone(personaTools), func(name string) bool { return orchestration[name] || tools.IsOperationEntry(name) })
}

func configuredToolSecrets(toolsCfg *appconfig.Tools) []string {
	if toolsCfg == nil {
		return nil
	}
	return toolsCfg.WebSearch.ConfiguredSecrets()
}

func newChannelEffects(db *sql.DB, logger *slog.Logger) (*effect.Executor, error) {
	if db == nil {
		return nil, errors.New("storage database is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	journal, err := effect.NewJournal(db, effect.Options{Logger: logger, Turns: turnstore.NewTurnStore(db)})
	if err != nil {
		return nil, err
	}
	return effect.NewExecutor(journal)
}

func stepToolCatalog(cfg *appconfig.Config) (*stepCatalog, error) {
	credentials := githubCredentials(cfg)
	if len(credentials) == 0 {
		return newStepCatalog(nil), nil
	}
	definitions, err := github.CheckedDefinitions(credentials)
	if err != nil {
		return nil, fmt.Errorf("prepare github step tools: %w", err)
	}
	return newStepCatalog(definitions), nil
}

func workflowValidationDeps(cfg *appconfig.Config) (*workflow.ValidationDeps, error) {
	catalog, err := stepToolCatalog(cfg)
	if err != nil {
		return nil, err
	}
	deps := &workflow.ValidationDeps{
		KnownTools:     stepToolNames(personaToolNames(cfg)),
		EffectfulTools: append(effectfulToolNames(), github.ToolNames()...),
		SinkTools:      catalog.sinkToolNames(),
		CheckToolCall:  catalog.CheckArguments,
	}
	if cfg.Workflows != nil {
		deps.DefaultStepTimeout = time.Duration(cfg.Workflows.DefaultStepTimeout)
	}
	registry, err := buildAgentRegistry(cfg)
	if err != nil {
		return deps, err
	}
	deps.Agents = registry
	deps.StepAllowed = stepCeiling(registry, catalog)
	return deps, nil
}

func stepCeiling(registry *auraagent.Registry, steps *stepCatalog) func(agentID string, class principal.Class, toolID string) (allowed, personaKnown bool) {
	return func(agentID string, class principal.Class, toolID string) (bool, bool) {
		persona, known := registry.Lookup(agentID)
		if !known {
			return false, false
		}
		tool, found := steps.Lookup(toolID)
		return found && persona.StepToolAllowed(class, tool), true
	}
}

func workflowDefinitionsDir(cfg *appconfig.Config) string {
	if cfg.Workflows == nil || cfg.Workflows.DefinitionsDir == "" {
		return ""
	}
	return cfg.Workflows.DefinitionsDir
}

type workflowOwnerRouteAdapter struct {
	route *approval.OwnerRoute
}

func newWorkflowOwnerRoute(route *approval.OwnerRoute) workflow.OwnerRouteHolder {
	if route == nil {
		return nil
	}
	return &workflowOwnerRouteAdapter{route: route}
}

func (a *workflowOwnerRouteAdapter) Hold(sessionID string, requester *workflow.StepRequester) func() {
	if a == nil || a.route == nil || requester == nil {
		return func() {}
	}
	return a.route.Hold(sessionID, &approval.Requester{
		Principal: requester.Principal,
		Class:     requester.Class,
		Persona:   requester.Persona,
	})
}

type replyTargeter interface {
	ReplyTarget(sessionID string) (access.HandoffTarget, bool)
}

type workflowReplyTargeterAdapter struct {
	target replyTargeter
}

func (a *workflowReplyTargeterAdapter) ReplyTarget(sessionID string) (workflow.ReplyTarget, bool) {
	target, found := a.target.ReplyTarget(sessionID)
	if !found || (target.Channel != principal.ChannelDiscord && target.Channel != principal.ChannelTelegram) {
		return workflow.ReplyTarget{}, false
	}
	return workflow.ReplyTarget{Channel: target.Channel, Account: target.Account, ChannelID: target.ChannelID, ThreadID: target.ThreadID}, true
}

func workflowReplyTargeters(ports []runtimechannelhost.ChannelPort) []workflow.ReplyTargeter {
	var targeters []workflow.ReplyTargeter
	for _, port := range ports {
		if targeter, ok := port.(replyTargeter); ok {
			targeters = append(targeters, &workflowReplyTargeterAdapter{target: targeter})
		}
	}
	return targeters
}

type proposalOfferer interface {
	OfferProposal(ctx context.Context, card approval.ProposalCard, decide approval.ProposalDecider) error
	ReplyTarget(sessionID string) (access.HandoffTarget, bool)
}

type workflowProposalOffererAdapter struct {
	offerer proposalOfferer
}

func (a *workflowProposalOffererAdapter) OfferProposal(ctx context.Context, card workflow.ProposalCard, decide func(ctx context.Context, approved bool) (string, error)) error {
	return a.offerer.OfferProposal(ctx, approval.ProposalCard{ID: card.ID, Plan: card.Plan, Tainted: card.Tainted}, decide)
}

func (a *workflowProposalOffererAdapter) ServesSession(sessionID string) bool {
	_, ok := a.offerer.ReplyTarget(sessionID)
	return ok
}

func workflowProposalOfferers(ports []runtimechannelhost.ChannelPort) []workflow.ProposalOfferer {
	var offerers []workflow.ProposalOfferer
	for _, port := range ports {
		if offerer, ok := port.(proposalOfferer); ok {
			offerers = append(offerers, &workflowProposalOffererAdapter{offerer: offerer})
		}
	}
	return offerers
}

var agentStepToolNames = []string{filesystem.ReadFileToolName, filesystem.ListDirToolName, search.ToolName, fetch.ToolName}

type readOnlyAgentStepTools struct {
	inner runtimeadk.BuiltinToolExecutor
}

func newReadOnlyAgentStepTools(inner runtimeadk.BuiltinToolExecutor) (*readOnlyAgentStepTools, error) {
	if inner == nil {
		return nil, invalidOptions("the agent step tools need the surface tool executor")
	}
	return &readOnlyAgentStepTools{inner: inner}, nil
}

func (r *readOnlyAgentStepTools) Definitions() []runtimeadk.BuiltinToolDefinition {
	registered := r.inner.Definitions()
	offered := make([]runtimeadk.BuiltinToolDefinition, 0, len(agentStepToolNames))
	for i := range registered {
		if slices.Contains(agentStepToolNames, registered[i].Name) {
			offered = append(offered, registered[i])
		}
	}
	return offered
}

func (r *readOnlyAgentStepTools) Execute(ctx context.Context, request *runtimeadk.BuiltinToolRequest) (json.RawMessage, error) {
	if request == nil {
		return nil, invalidOptions("builtin tool request must not be nil")
	}
	if !slices.Contains(agentStepToolNames, request.ToolName) {
		return nil, toolbroker.Errorf(toolbroker.ResultPolicyDenied, "tool %q is not available to workflow agent steps", request.ToolName)
	}
	forwarded := *request
	forwarded.Unattended = true
	return r.inner.Execute(ctx, &forwarded)
}

func toolBudgetFromRuntime(cfg runtimeengine.Config) runtimeadk.ToolBudget {
	return runtimeadk.ToolBudget{
		AnswerReserve:            time.Duration(cfg.AnswerReserve),
		MaxConsecutiveToolErrors: cfg.MaxConsecutiveToolErrors,
		MaxToolErrorsPerTurn:     cfg.MaxToolErrorsPerTurn,
	}
}

func effectiveToolBudget(budget runtimeadk.ToolBudget) runtimeadk.ToolBudget {
	if budget == (runtimeadk.ToolBudget{}) {
		return toolBudgetFromRuntime(runtimeengine.Default())
	}
	return budget
}

type agentStepExecutorInput struct {
	models     runtimeadk.ModelSource
	modelName  string
	sessions   runtimeadk.SessionPort
	builtin    runtimeadk.BuiltinToolExecutor
	ledger     *usage.Ledger
	registry   *auraagent.Registry
	routeModel func(route string) (string, error)
	toolBudget runtimeadk.ToolBudget
	logger     *slog.Logger
}

func newAgentStepExecutor(in *agentStepExecutorInput) (*runtimeadk.ADKExecutor, error) {
	opts := []runtimeadk.ExecutorOption{
		runtimeadk.WithToolBudget(effectiveToolBudget(in.toolBudget)),
		runtimeadk.WithBudgetLedger(in.ledger, "primary"),
		runtimeadk.WithAgentResolver(in.registry, in.routeModel),
	}
	readOnly, err := newReadOnlyAgentStepTools(in.builtin)
	if err != nil {
		return nil, err
	}
	if len(readOnly.Definitions()) > 0 {
		opts = append(opts, runtimeadk.WithBuiltinToolExecutor(readOnly))
	}
	return runtimeadk.NewADKExecutor("aura", in.modelName, in.models, in.sessions, terminal.DenyPolicy{}, nil, in.logger, opts...)
}

func (b *build) newAgentStepExecutor() (*runtimeadk.ADKExecutor, error) {
	return newAgentStepExecutor(&agentStepExecutorInput{
		models: b.models, modelName: b.primaryModel(), sessions: b.sessions, builtin: b.builtin,
		ledger: b.ledger, registry: b.registry, routeModel: b.routeModel,
		toolBudget: toolBudgetFromRuntime(b.cfg.Runtime), logger: b.logger,
	})
}

type workflowSessionTextAppender struct {
	db *sql.DB
}

func (a *workflowSessionTextAppender) AppendSessionText(ctx context.Context, sessionID, id, author, text string, at time.Time) error {
	return appendSessionText(ctx, a.db, sessionID, id, author, text, at)
}

type workflowCompletionDeliverer struct {
	cfg             *appconfig.Config
	deliver         func(ctx context.Context, account *discord.Account, channelID, text string) error
	deliverTelegram func(ctx context.Context, account *telegram.Account, chatID, threadID, text string) error
}

func (d *workflowCompletionDeliverer) DeliverReply(ctx context.Context, target *workflow.ReplyTarget, text string) error {
	switch target.Channel {
	case principal.ChannelDiscord:
		account, ok := discordAccount(d.cfg, target.Account)
		if !ok {
			return fmt.Errorf("discord account %q is not configured", target.Account)
		}
		return d.deliver(ctx, account, target.ChannelID, text)
	case principal.ChannelTelegram:
		account, ok := telegramAccount(d.cfg, target.Account)
		if !ok {
			return fmt.Errorf("telegram account %q is not configured", target.Account)
		}
		return d.deliverTelegram(ctx, account, target.ChannelID, target.ThreadID, text)
	default:
		return fmt.Errorf("reply channel %q is not supported", target.Channel)
	}
}

type behaviorModels struct {
	defaultModel string
	route        func(string) (string, error)
	models       runtimeadk.ModelSource
	toolBudget   runtimeadk.ToolBudget
}

func routedBehaviorModels(cfg *appconfig.Config, models runtimeadk.ModelSource) (behaviorModels, error) {
	defaultModel := cfg.Models.Definitions["primary"].Model
	route, err := model.RouteModelName(cfg.Models, cfg.ModelRoutes, defaultModel)
	if err != nil {
		return behaviorModels{}, err
	}
	return behaviorModels{defaultModel: defaultModel, route: route, models: models, toolBudget: toolBudgetFromRuntime(cfg.Runtime)}, nil
}

func (m behaviorModels) newExecutor(sessions runtimeadk.SessionPort, definition *auraagent.Definition) (*runtimeadk.ADKExecutor, error) {
	return runtimeadk.NewADKExecutor("aura", m.defaultModel, m.models, sessions, personaEvalDenyAll{}, nil, nil,
		runtimeadk.WithToolBudget(effectiveToolBudget(m.toolBudget)),
		runtimeadk.WithBuiltinToolExecutor(catalogExecutor{}),
		runtimeadk.WithAgentResolver(&staticResolver{definition: *definition}, m.route),
		runtimeadk.WithOwnerContext(personaeval.MarkerOwnerContext),
		runtimeadk.WithContextProvider(profileMarkerProvider{text: personaeval.MarkerProfile}),
		runtimeadk.WithContextProvider(markerProvider{text: personaeval.MarkerSkill}),
		runtimeadk.WithContextProvider(recallMarkerProvider{text: personaeval.MarkerRecall}),
	)
}

func (m behaviorModels) converse(ctx context.Context, definition *auraagent.Definition, class principal.Class, turns []string) ([]personaeval.Exchange, error) {
	return personaeval.WithEvalDB(ctx, "discord:eval", func(db *sql.DB) ([]personaeval.Exchange, error) {
		events := turnstore.NewEventStore(db)
		executor, err := m.newExecutor(turnstore.NewSessionService(db), definition)
		if err != nil {
			return nil, err
		}
		transcript := make([]personaeval.Exchange, 0, len(turns))
		for i, text := range turns {
			req := &turn.TurnRequest{
				TurnID: fmt.Sprintf("persona-eval-behavior-%d", i+1), SessionID: personaeval.SessionID, PrincipalID: "discord:eval", PrincipalClass: class,
				Origin: "discord", AgentID: definition.ID, Parts: []runtimeingress.InputPart{{Text: text}},
			}
			var produced []turn.Event
			for event, err := range executor.Execute(ctx, req) {
				if err != nil {
					return nil, fmt.Errorf("persona turn %d: %w", i+1, err)
				}
				if err := persistEvalEvent(ctx, events, req, &event); err != nil {
					return nil, err
				}
				produced = append(produced, event)
			}
			transcript = append(transcript, personaeval.Exchange{User: text, Assistant: strings.Join(runtimeengine.DeliveryTexts(produced), "\n")})
		}
		return transcript, nil
	})
}

func persistEvalEvent(ctx context.Context, events turnstore.EventStore, req *turn.TurnRequest, event *turn.Event) error {
	if event.ID == "" {
		event.ID = runtimeengine.NewTurnID()
	}
	event.SessionID, event.TurnID = req.SessionID, req.TurnID
	if event.Author == "" {
		event.Author = req.PrincipalID
	}
	if event.SchemaVersion == 0 {
		event.SchemaVersion = 1
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	_, err := events.AppendSequenced(ctx, req.SessionID, event)
	return err
}

func (m behaviorModels) judge(route string) personaeval.Judge {
	return func(ctx context.Context, prompt string) (string, error) {
		name, err := m.route(route)
		if err != nil {
			return "", err
		}
		llm, err := m.models.LLM(ctx, name)
		if err != nil {
			return "", fmt.Errorf("resolve judge model %q: %w", name, err)
		}
		var temperature float32
		var collected strings.Builder
		for response, err := range llm.GenerateContent(ctx, &adkmodel.LLMRequest{
			Contents: []*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{{Text: prompt}}}},
			Config:   &genai.GenerateContentConfig{Temperature: &temperature, MaxOutputTokens: judgeMaxOutputTokens},
		}, false) {
			if err != nil {
				return "", err
			}
			if response == nil || response.Content == nil {
				continue
			}
			for _, part := range response.Content.Parts {
				if part == nil {
					continue
				}
				if collected.Len()+len(part.Text) > personaeval.JudgeMaxOutputBytes {
					return "", errors.New("judge output exceeds the bound")
				}
				collected.WriteString(part.Text)
			}
		}
		return collected.String(), nil
	}
}

type personaEvalModel struct {
	mu       sync.Mutex
	requests []*adkmodel.LLMRequest
}

func (m *personaEvalModel) Name() string { return "persona-eval" }

func (m *personaEvalModel) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	m.mu.Lock()
	m.requests = append(m.requests, req)
	m.mu.Unlock()
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(&adkmodel.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "ok"}}},
			TurnComplete: true,
		}, nil)
	}
}

type staticResolver struct {
	definition auraagent.Definition
}

func (r *staticResolver) Resolve([]string, *string) (auraagent.Definition, error) {
	return r.definition, nil
}

type profileMarkerProvider struct {
	text string
}

func (p profileMarkerProvider) TurnContext(_ context.Context, req *turn.TurnRequest) ([]turn.ContextBlock, error) {
	if req == nil || req.PrincipalClass != principal.ClassOwner {
		return nil, nil
	}
	return []turn.ContextBlock{{
		Order:   turn.ContextOrderProfile,
		Content: p.text,
	}}, nil
}

func (profileMarkerProvider) Required(*turn.TurnRequest) bool {
	return false
}

type markerProvider struct {
	text string
}

func (p markerProvider) TurnContext(_ context.Context, req *turn.TurnRequest) ([]turn.ContextBlock, error) {
	if req == nil || req.PrincipalClass != principal.ClassOwner {
		return nil, nil
	}
	return []turn.ContextBlock{{Order: turn.ContextOrderSkills, Content: p.text}}, nil
}

func (markerProvider) Required(_ *turn.TurnRequest) bool {
	return false
}

type recallMarkerProvider struct {
	text string
}

func (p recallMarkerProvider) TurnContext(_ context.Context, req *turn.TurnRequest) ([]turn.ContextBlock, error) {
	if req != nil && req.UntrustedContext != nil {
		return nil, nil
	}
	recall := &turn.UntrustedRecall{Query: "eval", Trust: policyapproval.TrustUntrustedExternal}
	if req != nil && req.PrincipalID == principal.OwnerID {
		recall.Documents = []turn.UntrustedDocument{
			{ID: "doc-eval", SessionID: personaeval.SessionID, FromSequence: 1, ToSequence: 1, Trust: policyapproval.TrustUntrustedExternal, Content: p.text},
		}
	}
	return []turn.ContextBlock{{
		Order:     turn.ContextOrderRecall,
		Placement: turn.ContextPlacementUserPart,
		Content:   turn.RenderUntrustedRecall(recall),
		Caveat:    turn.RecallEvidenceCaveat,
	}}, nil
}

func (recallMarkerProvider) Required(*turn.TurnRequest) bool { return false }

type catalogExecutor struct{}

func (catalogExecutor) Definitions() []runtimeadk.BuiltinToolDefinition {
	return catalogDefinitions()
}

func (catalogExecutor) Execute(context.Context, *runtimeadk.BuiltinToolRequest) (json.RawMessage, error) {
	return nil, errors.New("persona eval does not execute tools")
}

type personaEvalDenyAll struct{}

func (personaEvalDenyAll) Evaluate(context.Context, *policyapproval.ToolRequest) (policyapproval.PolicyDecision, error) {
	return policyapproval.PolicyDecision{Outcome: policyapproval.OutcomeDeny}, nil
}

type personaCapturer struct {
	budget runtimeadk.ToolBudget
}

func newPersonaCapturer(cfg *appconfig.Config) personaCapturer {
	if cfg == nil {
		return personaCapturer{budget: toolBudgetFromRuntime(runtimeengine.Default())}
	}
	return personaCapturer{budget: toolBudgetFromRuntime(cfg.Runtime)}
}

func (c personaCapturer) capture(ctx context.Context, definition *auraagent.Definition, class principal.Class) (personaeval.Capture, error) {
	return capturePersonaTurnWithBudget(ctx, definition, class, c.budget)
}

func (c personaCapturer) newExecutor(sessions runtimeadk.SessionPort, evalModel adkmodel.LLM, definition *auraagent.Definition) (*runtimeadk.ADKExecutor, error) {
	return newCaptureExecutor(sessions, evalModel, definition, c.budget)
}

func capturePersonaTurn(ctx context.Context, definition *auraagent.Definition, class principal.Class) (personaeval.Capture, error) {
	return newPersonaCapturer(nil).capture(ctx, definition, class)
}

func capturePersonaTurnWithBudget(ctx context.Context, definition *auraagent.Definition, class principal.Class, budget runtimeadk.ToolBudget) (personaeval.Capture, error) {
	principalID := "discord:eval"
	if class == principal.ClassOwner {
		principalID = principal.OwnerID
	}
	return personaeval.WithEvalDB(ctx, principalID, func(db *sql.DB) (personaeval.Capture, error) {
		return captureWith(ctx, db, definition, class, principalID, budget)
	})
}

func newCaptureExecutor(sessions runtimeadk.SessionPort, evalModel adkmodel.LLM, definition *auraagent.Definition, budget runtimeadk.ToolBudget) (*runtimeadk.ADKExecutor, error) {
	name := evalModel.Name()
	evaluated := *definition
	evaluated.ModelRoute = ""
	return runtimeadk.NewADKExecutor("aura", name, model.Set{name: evalModel}, sessions, personaEvalDenyAll{}, nil, nil,
		runtimeadk.WithToolBudget(effectiveToolBudget(budget)),
		runtimeadk.WithBuiltinToolExecutor(catalogExecutor{}),
		runtimeadk.WithAgentResolver(&staticResolver{definition: evaluated}, nil),
		runtimeadk.WithOwnerContext(personaeval.MarkerOwnerContext),
		runtimeadk.WithContextProvider(profileMarkerProvider{text: personaeval.MarkerProfile}),
		runtimeadk.WithContextProvider(markerProvider{text: personaeval.MarkerSkill}),
		runtimeadk.WithContextProvider(recallMarkerProvider{text: personaeval.MarkerRecall}),
	)
}

func captureWith(ctx context.Context, db *sql.DB, definition *auraagent.Definition, class principal.Class, principalID string, budget runtimeadk.ToolBudget) (personaeval.Capture, error) {
	sessions := turnstore.NewSessionService(db)
	evalModel := &personaEvalModel{}
	executor, err := newCaptureExecutor(sessions, evalModel, definition, budget)
	if err != nil {
		return personaeval.Capture{}, err
	}
	req := &turn.TurnRequest{
		TurnID: "persona-eval-turn", SessionID: personaeval.SessionID, PrincipalID: principalID, PrincipalClass: class,
		Origin: "discord", AgentID: definition.ID, Parts: []runtimeingress.InputPart{{Text: "Tell me everything you know about the owner."}},
	}
	for _, err := range executor.Execute(ctx, req) {
		if err != nil {
			return personaeval.Capture{}, fmt.Errorf("persona eval turn: %w", err)
		}
	}
	evalModel.mu.Lock()
	defer evalModel.mu.Unlock()
	if len(evalModel.requests) == 0 {
		return personaeval.Capture{}, errors.New("persona eval turn made no model request")
	}
	return summarizeRequest(evalModel.requests[0]), nil
}

func summarizeRequest(req *adkmodel.LLMRequest) personaeval.Capture {
	var names []string
	var text strings.Builder
	if req.Config != nil {
		for _, declared := range req.Config.Tools {
			for _, function := range declared.FunctionDeclarations {
				names = append(names, function.Name)
			}
		}
		if req.Config.SystemInstruction != nil {
			for _, part := range req.Config.SystemInstruction.Parts {
				text.WriteString(part.Text)
				text.WriteString("\n")
			}
		}
	}
	for _, content := range req.Contents {
		for _, part := range content.Parts {
			text.WriteString(part.Text)
			text.WriteString("\n")
		}
	}
	slices.Sort(names)
	if names == nil {
		names = []string{}
	}
	return personaeval.Capture{Tools: names, Text: text.String()}
}
