package codeexec

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/anggasct/aura/internal/modules/codeexec/interpreter"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/storage"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

const RunsDirName = "interpreter-runs"

type ModuleDeps struct {
	Config       *Config
	DataRoot     string
	Manifest     *interpreter.Manifest
	Artifacts    storage.ArtifactStore
	Observer     Observer
	Prober       interpreter.Prober
	CheckTimeout time.Duration
	Logger       *slog.Logger
}

type Module struct {
	store        *interpreter.Store
	verified     *interpreter.VerifiedSet
	runner       *Runner
	verifier     *interpreter.Verifier
	checkTimeout time.Duration
	logger       *slog.Logger
}

func New(ctx context.Context, deps *ModuleDeps) (*Module, error) {
	if deps == nil || deps.Config == nil || deps.Artifacts == nil || deps.DataRoot == "" {
		return nil, Errorf(ErrorCodeInvalidArgument, "codeexec module dependencies, configuration, data root and artifact store are required")
	}
	manifest := deps.Manifest
	if manifest == nil {
		var err error
		manifest, err = interpreter.DefaultManifest()
		if err != nil {
			return nil, err
		}
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	absDataRoot, err := filepath.Abs(deps.DataRoot)
	if err != nil {
		return nil, wrapf(ErrorCodeRunFailed, pathFree(err), "resolve data root")
	}
	store, err := interpreter.NewStore(absDataRoot, manifest)
	if err != nil {
		return nil, err
	}
	verified, err := interpreter.NewVerifiedSet(store.Manifest())
	if err != nil {
		return nil, err
	}
	runner, err := NewRunner(&Options{
		Store:     store,
		Verified:  verified,
		Artifacts: storageArtifactSink{store: deps.Artifacts},
		Observer:  deps.Observer,
		RunRoot:   filepath.Join(absDataRoot, RunsDirName),
		Limits:    deps.Config.Limits(),
	})
	if err != nil {
		return nil, err
	}
	removed, err := runner.SweepStale(ctx)
	if err != nil {
		return nil, err
	}
	if removed > 0 {
		logger.InfoContext(ctx, "interpreter stale run directories removed", "component", ComponentInterpreter, "removed", removed)
	}
	prober := deps.Prober
	if prober == nil {
		prober = runner
	}
	verifier, err := interpreter.NewVerifier(store, verified, interpreter.WithProber(prober))
	if err != nil {
		return nil, err
	}
	return &Module{
		store:        store,
		verified:     verified,
		runner:       runner,
		verifier:     verifier,
		checkTimeout: deps.CheckTimeout,
		logger:       logger,
	}, nil
}

func (m *Module) Tools() []toolbroker.ToolRegistration {
	if m == nil || m.runner == nil {
		return nil
	}
	return []toolbroker.ToolRegistration{{
		Definition:    ToolDefinition(),
		Adapter:       m.runner.Adapter(),
		Rule:          ToolRule(),
		ArgumentCheck: m.runner.ArgumentCheck,
	}}
}

func (m *Module) HealthChecks() []health.RegisteredCheck {
	if m == nil || m.verified == nil {
		return nil
	}
	return []health.RegisteredCheck{{
		ID: ComponentInterpreter,
		Checker: HealthChecker{Runtimes: func(context.Context) ([]RuntimeHealth, error) {
			return healthRuntimes(m.verified.Statuses()), nil
		}},
		Timeout:     m.checkTimeout,
		Remediation: RemediationInstallInterpreter,
	}}
}

func (m *Module) VerifyRun(ctx context.Context) {
	if m == nil || m.verifier == nil {
		return
	}
	if err := m.verifier.Run(ctx); err != nil {
		if ctx.Err() == nil {
			m.logger.WarnContext(ctx, "interpreter runtime verification failed", "component", ComponentInterpreter, "error", err)
		}
		return
	}
	for _, verification := range m.verified.Statuses() {
		level := slog.LevelInfo
		if verification.State != interpreter.StateInstalled {
			level = slog.LevelWarn
		}
		m.logger.Log(ctx, level, "interpreter runtime verified",
			"component", ComponentInterpreter, "runtime", verification.Runtime, "version", verification.Version,
			"state", string(verification.State), "reason", string(verification.Reason))
	}
}

func (m *Module) Store() *interpreter.Store {
	if m == nil {
		return nil
	}
	return m.store
}

func (m *Module) Verified() *interpreter.VerifiedSet {
	if m == nil {
		return nil
	}
	return m.verified
}

func (m *Module) Runner() *Runner {
	if m == nil {
		return nil
	}
	return m.runner
}

type storageArtifactSink struct {
	store storage.ArtifactStore
}

func (a storageArtifactSink) Put(ctx context.Context, content io.Reader, meta *ArtifactMeta) (ArtifactReceipt, error) {
	ref, err := a.store.Put(ctx, content, &storage.ArtifactMetadata{
		ID: meta.ID, SessionID: meta.SessionID, Filename: meta.Filename, MediaType: meta.MediaType, Metadata: meta.Metadata,
	})
	if err != nil {
		if fault.HasCode(err, storage.ErrorCodeArtifactQuotaExceeded) {
			return ArtifactReceipt{}, Errorf(ErrorCodeArtifactQuota, "the artifact store quota is exhausted")
		}
		return ArtifactReceipt{}, errors.New("the artifact store refused the artifact")
	}
	return ArtifactReceipt{ID: ref.ID, Digest: ref.BlobDigest, SizeBytes: ref.SizeBytes}, nil
}
