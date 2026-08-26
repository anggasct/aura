package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/channel/terminal"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/sandbox"
)

type usageError struct {
	err error
}

func (e *usageError) Error() string { return e.err.Error() }

func (e *usageError) Unwrap() error { return e.err }

// exitCodeError carries an explicit process exit code for commands whose
// contract defines more states than ok/usage-failure (diagnostics exit
// codes, for example).
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

func newRootCmd() *cobra.Command {
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

	root.AddCommand(newVersionCmd(), newServerCmd(gf), newChatCmd(gf), newExecCmd(), newStorageCmd(gf), newUsageCmd(gf), newEffectsCmd(gf), newStatusCmd(gf, sandbox.Negotiate), newDoctorCmd(gf, sandbox.Negotiate))
	return root
}

func Execute() int {
	return ExecuteContext(context.Background())
}

func ExecuteContext(ctx context.Context, args ...string) int {
	effective := args
	if len(effective) == 0 {
		effective = os.Args[1:]
	}
	root := newRootCmd()
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return &usageError{err}
	})
	root.SetArgs(effective)
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "aura:", err)
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
		if _, _, findErr := root.Find(effective); findErr != nil {
			return 2
		}
		return 1
	}
	return 0
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
