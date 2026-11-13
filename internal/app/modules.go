package app

import (
	"context"
	"time"

	toolsbuiltin "github.com/anggasct/aura/internal/modules/builtin"
	"github.com/anggasct/aura/internal/modules/codeexec/interpreter"
	"github.com/anggasct/aura/internal/modules/memory"
	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/server"
	"github.com/anggasct/aura/internal/platform/telemetry"
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
	runtime, enabled, err := buildKnowledgeRuntime(ctx, b.cfg, b.registry)
	if err != nil {
		return err
	}
	if !enabled {
		return nil
	}
	if err := b.group.Defer("knowledge", func(context.Context) error {
		runtime.close()
		return nil
	}); err != nil {
		runtime.close()
		return err
	}
	runtime.attachSearch(b.builtin)
	b.execOpts = append(b.execOpts, runtimeadk.WithKnowledgeContext(runtime.provider))
	return nil
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
	specs, err := workflow.LoadDefinitionsDir(workflowDefinitionsDir(cfg))
	if err != nil {
		return err
	}
	deps, err := workflowValidationDeps(cfg)
	if err != nil {
		return err
	}
	options := &workflow.Options{
		MaxConcurrentSteps: workflowMaxConcurrentSteps(cfg),
		Logger:             b.logger,
	}
	if cfg.Workflows != nil {
		options.GeneratedBudget = workflow.Budget{MaxSteps: cfg.Workflows.Generated.MaxSteps, MaxTokens: cfg.Workflows.Generated.MaxTokens}
	}
	if host && cfg.Tools != nil {
		tools, err := newWorkflowToolRunner(b.result, db, b.logger)
		if err != nil {
			return err
		}
		options.Tools = tools
	}
	runtime, err := b.durableRuntime()
	if err != nil {
		return err
	}
	built := workflow.NewInterpreter(workflow.NewStore(db), runtime, options)
	routines, err := workflow.NewStore(db).Routines(ctx)
	if err != nil {
		return err
	}
	if err := built.Install(ctx, specs, deps); err != nil {
		return err
	}
	if err := built.LoadAll(ctx, routines, deps); err != nil {
		return err
	}
	b.interpreter = built
	return nil
}
