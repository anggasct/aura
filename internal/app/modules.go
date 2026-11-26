package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"time"

	toolsbuiltin "github.com/anggasct/aura/internal/modules/builtin"
	"github.com/anggasct/aura/internal/modules/codeexec/interpreter"
	personaeval "github.com/anggasct/aura/internal/modules/eval/persona"
	"github.com/anggasct/aura/internal/modules/github"
	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/modules/memory"
	"github.com/anggasct/aura/internal/modules/profile"
	"github.com/anggasct/aura/internal/modules/skills"
	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/secret"
	"github.com/anggasct/aura/internal/platform/server"
	"github.com/anggasct/aura/internal/platform/telemetry"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeadk "github.com/anggasct/aura/internal/platform/turn/adk"
	runtimeengine "github.com/anggasct/aura/internal/platform/turn/engine"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
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
		b.buildMemory,
		b.buildProfile,
		b.buildSkills,
		b.buildKnowledge,
		b.buildGitHub,
		b.buildInterpreter,
		b.buildPersonaEval,
	}
	for _, step := range steps {
		if err := step(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (b *build) addHealthCheck(check *health.RegisteredCheck) {
	if b.onServer() {
		b.server.checks = append(b.server.checks, *check)
	}
}

func (b *build) registerModule(name string, mod any) error {
	if tp, ok := mod.(toolbroker.ToolProvider); ok && b.builtin != nil {
		if err := b.builtin.Broker().RegisterProvider(tp); err != nil {
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
	if th, ok := mod.(turn.TurnHook); ok {
		b.turnHooks = append(b.turnHooks, th)
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
	if !cfg.Memory.Enabled {
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
	if cfg.Profile == nil || !cfg.Profile.Enabled {
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
		Scanner:      buildSecretVault(cfg),
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

func (b *build) buildKnowledge(ctx context.Context) error {
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
	dbPath, _, _, err := storagePaths(b.cfg)
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
	if b.builtin == nil || !interpreterEnabled(b.result.CapabilityReport) {
		return nil
	}
	manifest, err := interpreter.DefaultManifest()
	if err != nil {
		return err
	}
	runtime, err := newInterpreterRuntime(cfg, manifest)
	if err != nil {
		return err
	}
	var observer toolsbuiltin.CodeObserver
	if b.onServer() {
		recorder, err := telemetry.NewCodeRecorder(b.meters())
		if err != nil {
			return err
		}
		observer = codeObserver(recorder)
	}
	if err := runtime.enableExecution(ctx, cfg, b.builtin.Artifacts(), observer, logger); err != nil {
		return err
	}
	runtime.attach(b.builtin)
	check := runtime.check(cfg)
	b.addHealthCheck(&check)
	return b.group.Add(server.Background("interpreter_verification", func(ctx context.Context) {
		runtime.run(ctx, logger)
	}))
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
	if host && b.builtin != nil {
		modDeps.Broker = b.builtin.Broker()
		modDeps.RunSessions = b.sessions
		modDeps.Registry = b.registry
		modDeps.Appender = &workflowSessionTextAppender{db: db}
		modDeps.Deliver = &workflowCompletionDeliverer{cfg: cfg, deliver: deliverText}
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
	models := behaviorModels{defaultModel: b.primaryModel(), route: b.routeModel, models: b.models}
	mod, err := personaeval.New(&personaeval.ModuleDeps{
		DB:           b.db,
		Registry:     b.registry,
		ConfigPath:   b.result.Path,
		Capturer:     capturePersonaTurn,
		Converse:     models.converse,
		JudgeFor:     models.judge,
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
