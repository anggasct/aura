package app

import (
	"context"
	"encoding/json"
	"io"
	"time"

	toolsbuiltin "github.com/anggasct/aura/internal/modules/builtin"
	"github.com/anggasct/aura/internal/modules/codeexec/interpreter"
	"github.com/anggasct/aura/internal/modules/github"
	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/modules/memory"
	"github.com/anggasct/aura/internal/modules/skills"
	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/server"
	"github.com/anggasct/aura/internal/platform/telemetry"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/platform/turn/adk"
	"github.com/anggasct/aura/internal/platform/turn/engine"
	"github.com/anggasct/aura/internal/platform/turn/store"
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
	runs, err := workflow.NewStore(db, b.turns)
	if err != nil {
		return err
	}
	if host && b.builtin != nil {
		input := &workflowToolRunnerInput{
			Executor: b.builtin, Sessions: b.sessions, Registry: b.registry, Runs: runs, Logger: b.logger,
		}
		if b.server != nil {
			input.Route = b.server.workflowRoute
		}
		tools, err := newWorkflowToolRunner(input)
		if err != nil {
			return err
		}
		options.Tools = tools
	}
	runtime, err := b.durableRuntime()
	if err != nil {
		return err
	}
	built := workflow.NewInterpreter(runs, runtime, options)
	routines, err := runs.Routines(ctx)
	if err != nil {
		return err
	}
	if err := built.Install(ctx, specs, deps); err != nil {
		return err
	}
	quarantined, err := built.LoadRoutines(ctx, routines, deps)
	if err != nil {
		return err
	}
	for i := range quarantined {
		b.logger.WarnContext(ctx, "workflow routine does not compile and is quarantined", "component", "workflow",
			"definition_id", quarantined[i].ID, "version", quarantined[i].Version, "error", quarantined[i].Err)
	}
	b.quarantined = quarantined
	if b.onServer() {
		check := workflowDefinitionsCheck(b.cfg, b.registry, quarantined)
		b.addHealthCheck(&check)
	}
	b.interpreter = built
	return nil
}
