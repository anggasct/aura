package app

import (
	"context"
	"io"
	"time"

	toolsbuiltin "github.com/anggasct/aura/internal/modules/builtin"
	"github.com/anggasct/aura/internal/modules/codeexec/interpreter"
	"github.com/anggasct/aura/internal/modules/github"
	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/modules/memory"
	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/server"
	"github.com/anggasct/aura/internal/platform/telemetry"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/platform/turn/adk"
	"github.com/anggasct/aura/internal/platform/turn/engine"
)

type moduleParts struct {
	memoryHook    postTurnHook
	profileHook   postTurnHook
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
	if hp, ok := mod.(health.HealthProvider); ok {
		for _, check := range hp.HealthChecks() {
			if check.Timeout == 0 {
				check.Timeout = time.Duration(b.cfg.Health.CheckTimeout)
			}
			b.addHealthCheck(&check)
		}
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
	runtime, err := buildMemoryRuntime(ctx, cfg, b.db, b.registry, b.models, logger, observer)
	if err != nil {
		return err
	}
	b.execOpts = append(b.execOpts, runtimeadk.WithRecallProvider(runtime.provider))
	b.modules.memoryHook = buildMemoryHook(runtime.service, runtime.model, runtime.summarizer, logger)
	if !b.onServer() {
		return nil
	}
	return b.group.Add(server.Background("memory_purge", func(ctx context.Context) {
		runtime.service.RunPurgeLoop(ctx, time.Duration(cfg.Memory.PurgeInterval), cfg.Memory.PurgeLimit)
	}))
}

func (b *build) buildProfile(ctx context.Context) error {
	cfg := b.cfg
	if cfg.Profile == nil || !cfg.Profile.Enabled {
		return nil
	}
	runtime, err := buildProfileRuntime(ctx, cfg, b.db, b.models, b.meters(), b.logger)
	if err != nil {
		return err
	}
	b.execOpts = append(b.execOpts, runtimeadk.WithProfileContext(runtime))
	b.modules.profileHook = runtime.hook
	if err := b.group.Add(server.Background("profile_extraction", runtime.run)); err != nil {
		return err
	}
	check := runtime.check(cfg)
	b.addHealthCheck(&check)
	return nil
}

func (b *build) buildSkills(ctx context.Context) error {
	if b.cfg.Skills == nil || !b.cfg.Skills.Enabled {
		return nil
	}
	runtime, err := buildSkillRuntime(ctx, b.cfg.Skills, b.db, b.logger)
	if err != nil {
		return err
	}
	b.execOpts = append(b.execOpts, runtimeadk.WithSkillContext(runtime.context))
	b.modules.skillRenderer = runtime.context
	b.modules.skills = runtime.bridge
	return nil
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
