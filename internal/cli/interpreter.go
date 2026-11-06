package cli

import (
	"context"
	"log/slog"
	"time"

	"github.com/anggasct/aura/internal/capability"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/health"
	"github.com/anggasct/aura/internal/tools/interpreter"
)

const (
	codeInterpreterCapability = "code-interpreter"
	interpreterVerifyTimeout  = time.Minute
)

type interpreterRuntime struct {
	store    *interpreter.Store
	set      *interpreter.VerifiedSet
	verifier *interpreter.Verifier
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
	store, err := newInterpreterStore(cfg, manifest)
	if err != nil {
		return nil, err
	}
	set, err := interpreter.NewVerifiedSet(store.Manifest())
	if err != nil {
		return nil, err
	}
	verifier, err := interpreter.NewVerifier(store, set)
	if err != nil {
		return nil, err
	}
	return &interpreterRuntime{store: store, set: set, verifier: verifier}, nil
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
	store, err := newInterpreterStore(cfg, manifest)
	if err != nil {
		return nil, err
	}
	return []health.RegisteredCheck{{
		ID: health.ComponentInterpreter,
		Checker: health.InterpreterChecker{Runtimes: func(ctx context.Context) ([]health.InterpreterRuntime, error) {
			verifications, err := store.List(ctx)
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
