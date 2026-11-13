package app

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/channels/terminal"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/modules/codeexec/interpreter"
	"github.com/anggasct/aura/internal/platform/sandbox"
)

type usageError struct {
	err error
}

func (e *usageError) Error() string { return e.err.Error() }

func (e *usageError) Unwrap() error { return e.err }

type exitCodeError struct {
	code int
	err  error
}

func (e *exitCodeError) Error() string { return e.err.Error() }

func (e *exitCodeError) Unwrap() error { return e.err }

type globalFlags struct {
	configPath string
	logLevel   string
	logFormat  string
}

func NewRootCommand() *cobra.Command {
	gf := &globalFlags{}
	root := &cobra.Command{
		Use:           "aura",
		Short:         "Self-hosted personal AI agent",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	pf := root.PersistentFlags()
	pf.StringVar(&gf.configPath, "config", "", "path to config.yaml")
	pf.StringVar(&gf.logLevel, "log-level", "info", "log level (debug, info, warn, error)")
	pf.StringVar(&gf.logFormat, "log-format", "text", "log format (text, json)")

	root.AddCommand(newVersionCmd(), newServerCmd(gf), newChatCmd(gf), newExecCmd(), newStorageCmd(gf), newUsageCmd(gf), newEffectsCmd(gf), newAgentsCmd(gf), newPersonaCmd(gf), newBindingsCmd(gf), newSkillsCmd(gf), newProfileCmd(gf), newMemoryCmd(gf), newOwnerCmd(gf), newChildrenCmd(gf), newGrantsCmd(gf), newPairingCmd(gf), newPrincipalCmd(gf), newReplyCmd(gf), newHandoffCmd(gf), newWorkflowCmd(gf), newBroadcastCmd(gf), newScheduleCmd(gf), newStatusCmd(gf, sandbox.Negotiate), newDoctorCmd(gf, sandbox.Negotiate), newModelsCmd(gf), newSyncCmd(gf), newMCPCmd(gf), newPluginCmd(gf), newAPIKeyCmd(gf), newKBCmd(gf), newInterpreterCmd(gf, interpreterDeps{load: config.Load, manifest: interpreter.DefaultManifest, platformSupported: interpreter.HostSupported}))
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return &usageError{err}
	})
	return root
}

func ExitCode(root *cobra.Command, args []string, err error) int {
	if err == nil {
		return 0
	}
	var ue *usageError
	if errors.As(err, &ue) {
		return 2
	}
	if errors.Is(err, terminal.ErrInterrupted) {
		return 128 + int(syscall.SIGINT)
	}
	var coded *exitCodeError
	if errors.As(err, &coded) {
		return coded.code
	}
	if _, _, findErr := root.Find(args); findErr != nil {
		return 2
	}
	return 1
}

func logConfigResult(ctx context.Context, logger *slog.Logger, result *config.LoadResult) {
	if result.DefaultGenerated {
		logger.InfoContext(ctx, "generating default config", "component", "config", "file", filepath.Base(result.Path))
	}
	for _, key := range result.Warnings {
		logger.WarnContext(ctx, "unrecognized environment variable is ignored", "component", "config", "env", key)
	}
}

func resolveLogging(cmd *cobra.Command, cfg *config.Config) (level, format string) {
	level, format = cfg.Logging.Level, cfg.Logging.Format
	if f := cmd.Flags().Lookup("log-level"); f != nil && f.Changed {
		level = f.Value.String()
	}
	if f := cmd.Flags().Lookup("log-format"); f != nil && f.Changed {
		format = f.Value.String()
	}
	return level, format
}
