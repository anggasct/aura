package app

import (
	"context"
	"log/slog"

	"github.com/spf13/cobra"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/modules/codeexec"
	"github.com/anggasct/aura/internal/modules/codeexec/interpreter"
	"github.com/anggasct/aura/internal/platform/capability"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/logging"
	"github.com/anggasct/aura/internal/platform/telemetry"
)

type configLoader func(path string) (appconfig.LoadResult, error)

type interpreterDeps struct {
	load              configLoader
	manifest          func() (*interpreter.Manifest, error)
	platformSupported func() bool
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
	dataRoot, err := resolveDataRoot(cfg)
	if err != nil {
		return nil, err
	}
	return codeexec.HealthChecks(dataRoot, nil, codeexec.DefaultVerifyTimeout)
}

func interpreterOpener(gf *globalFlags) codeexec.Opener {
	return newInterpreterOpener(gf, interpreterDeps{
		load:              appconfig.Load,
		manifest:          interpreter.DefaultManifest,
		platformSupported: interpreter.HostSupported,
	})
}

func newInterpreterOpener(gf *globalFlags, deps interpreterDeps) codeexec.Opener {
	return func(cmd *cobra.Command, fn func(ctx context.Context, logger *slog.Logger, store *interpreter.Store) error) error {
		result, err := deps.load(gf.configPath)
		if err != nil {
			return err
		}
		level, format := resolveLogging(cmd, result.Config)
		logger, err := logging.Setup(level, format, cmd.ErrOrStderr())
		if err != nil {
			return err
		}
		logConfigResult(cmd.Context(), logger, &result)
		if err := requireInterpreterCapability(result.CapabilityReport, deps.platformSupported()); err != nil {
			return err
		}
		manifest, err := deps.manifest()
		if err != nil {
			return err
		}
		dataRoot, err := resolveDataRoot(result.Config)
		if err != nil {
			return err
		}
		store, err := interpreter.NewStore(dataRoot, manifest)
		if err != nil {
			return err
		}
		return fn(cmd.Context(), logger, store)
	}
}

func requireInterpreterCapability(report capability.Report, platformSupported bool) error {
	status, ok := report.Status(codeexec.CapabilityName)
	if !ok || !status.Compiled {
		return fault.New(capability.ErrorCodeCapabilityUnavailable, codeexec.CapabilityName+": capability is absent from this artifact")
	}
	if !platformSupported {
		return fault.New(capability.ErrorCodeCapabilityUnavailable, codeexec.CapabilityName+": runtimes need linux/amd64")
	}
	return nil
}
