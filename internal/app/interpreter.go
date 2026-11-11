package app

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/anggasct/aura/internal/config"
	toolsbuiltin "github.com/anggasct/aura/internal/modules/builtin"
	"github.com/anggasct/aura/internal/modules/codeexec/interpreter"
	"github.com/anggasct/aura/internal/platform/capability"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/telemetry"
	"github.com/anggasct/aura/internal/store"
)

const (
	codeInterpreterCapability = "code-interpreter"
	interpreterVerifyTimeout  = time.Minute
	interpreterRunsDirName    = "interpreter-runs"
)

type interpreterRuntime struct {
	store    *interpreter.Store
	set      *interpreter.VerifiedSet
	verifier *interpreter.Verifier
	runner   codeExecution
}

type codeExecution interface {
	toolsbuiltin.CodeRunner
	interpreter.Prober
}

func codeObserver(recorder *telemetry.CodeRecorder) toolsbuiltin.CodeObserver {
	return func(ctx context.Context, observation *toolsbuiltin.CodeObservation) {
		recorder.Record(ctx, &telemetry.CodeObservation{
			Runtime: observation.Runtime, ResultClass: string(observation.Class), Phase: string(observation.Phase),
			Compiled: observation.Compiled, BuildDuration: observation.Build.Duration, RunDuration: observation.Run.Duration,
			BuildCPU: observation.Build.CPUTime, RunCPU: observation.Run.CPUTime,
			OutputBytes: observation.StdoutBytes + observation.StderrBytes, ArtifactBytes: observation.ArtifactBytes,
		})
	}
}

func interpreterEnabled(report capability.Report) bool {
	status, ok := report.Status(codeInterpreterCapability)
	return ok && status.Enabled && status.Available
}

func newInterpreterStore(cfg *config.Config, manifest *interpreter.Manifest) (*interpreter.Store, error) {
	dataRoot, err := resolveDataRoot(cfg)
	if err != nil {
		return nil, err
	}
	return interpreter.NewStore(dataRoot, manifest)
}

func newInterpreterRuntime(cfg *config.Config, manifest *interpreter.Manifest) (*interpreterRuntime, error) {
	runtimeStore, err := newInterpreterStore(cfg, manifest)
	if err != nil {
		return nil, err
	}
	set, err := interpreter.NewVerifiedSet(runtimeStore.Manifest())
	if err != nil {
		return nil, err
	}
	verifier, err := interpreter.NewVerifier(runtimeStore, set)
	if err != nil {
		return nil, err
	}
	return &interpreterRuntime{store: runtimeStore, set: set, verifier: verifier}, nil
}

func (r *interpreterRuntime) enableExecution(ctx context.Context, cfg *config.Config, artifacts store.ArtifactStore, observer toolsbuiltin.CodeObserver, logger *slog.Logger) error {
	dataRoot, err := resolveDataRoot(cfg)
	if err != nil {
		return err
	}
	runner, err := toolsbuiltin.NewCodeRunner(&toolsbuiltin.CodeOptions{
		Store: r.store, Verified: r.set, Artifacts: artifacts, Observer: observer, RunRoot: filepath.Join(dataRoot, interpreterRunsDirName), Limits: &cfg.Interpreter,
	})
	if err != nil {
		return err
	}
	removed, err := runner.SweepStale(ctx)
	if err != nil {
		return err
	}
	if removed > 0 {
		logger.InfoContext(ctx, "interpreter stale run directories removed", "component", "interpreter", "removed", removed)
	}
	verifier, err := interpreter.NewVerifier(r.store, r.set, interpreter.WithProber(runner))
	if err != nil {
		return err
	}
	r.runner, r.verifier = runner, verifier
	return nil
}

func (r *interpreterRuntime) attach(builtin *toolsbuiltin.Executor) {
	if r.runner != nil {
		builtin.SetCodeRunner(r.runner)
	}
}

func (r *interpreterRuntime) run(ctx context.Context, logger *slog.Logger) {
	if err := r.verifier.Run(ctx); err != nil {
		if ctx.Err() == nil {
			logger.WarnContext(ctx, "interpreter runtime verification failed", "component", "interpreter", "error", err)
		}
		return
	}
	for _, verification := range r.set.Statuses() {
		level := slog.LevelInfo
		if verification.State != interpreter.StateInstalled {
			level = slog.LevelWarn
		}
		logger.Log(ctx, level, "interpreter runtime verified",
			"component", "interpreter", "runtime", verification.Runtime, "version", verification.Version,
			"state", string(verification.State), "reason", string(verification.Reason))
	}
}

func (r *interpreterRuntime) check(cfg *config.Config) health.RegisteredCheck {
	return health.RegisteredCheck{
		ID: health.ComponentInterpreter,
		Checker: health.InterpreterChecker{Runtimes: func(context.Context) ([]health.InterpreterRuntime, error) {
			return healthRuntimes(r.set.Statuses()), nil
		}},
		Timeout:     time.Duration(cfg.Health.CheckTimeout),
		Remediation: health.RemediationInstallInterpreter,
	}
}

func interpreterStatusChecks(cfg *config.Config, report capability.Report) ([]health.RegisteredCheck, error) {
	if !interpreterEnabled(report) {
		return nil, nil
	}
	manifest, err := interpreter.DefaultManifest()
	if err != nil {
		return nil, err
	}
	return interpreterManifestChecks(cfg, manifest)
}

func interpreterManifestChecks(cfg *config.Config, manifest *interpreter.Manifest) ([]health.RegisteredCheck, error) {
	runtimeStore, err := newInterpreterStore(cfg, manifest)
	if err != nil {
		return nil, err
	}
	return []health.RegisteredCheck{{
		ID: health.ComponentInterpreter,
		Checker: health.InterpreterChecker{Runtimes: func(ctx context.Context) ([]health.InterpreterRuntime, error) {
			verifications, err := runtimeStore.List(ctx)
			if err != nil {
				return nil, err
			}
			return healthRuntimes(verifications), nil
		}},
		Timeout:     interpreterVerifyTimeout,
		Remediation: health.RemediationInstallInterpreter,
	}}, nil
}

func healthRuntimes(verifications []interpreter.Verification) []health.InterpreterRuntime {
	out := make([]health.InterpreterRuntime, 0, len(verifications))
	for _, verification := range verifications {
		out = append(out, health.InterpreterRuntime{
			Runtime: verification.Runtime, Version: verification.Version,
			State: string(verification.State), Reason: string(verification.Reason),
		})
	}
	return out
}
