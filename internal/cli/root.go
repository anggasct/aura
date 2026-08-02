package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/config"
)

// usageError marks argument and flag errors so ExecuteContext can exit with
// code 2 instead of the runtime error code.
type usageError struct {
	err error
}

func (e *usageError) Error() string { return e.err.Error() }

func (e *usageError) Unwrap() error { return e.err }

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

	root.AddCommand(newVersionCmd(), newServerCmd(gf), newChatCmd(), newExecCmd(), newStorageCmd(gf))
	return root
}

func Execute() int {
	return ExecuteContext(context.Background())
}

// ExecuteContext runs the CLI with ctx, exiting 2 for usage errors, 1 for
// runtime errors, and 0 on success. When args is empty the process arguments
// are used.
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
		// An unknown subcommand is a usage error, but cobra surfaces it
		// from Execute with no marker. Classify it here: by this point
		// cobra's built-in commands (help, completion, __complete) are
		// registered, so a Find failure is a genuine unknown command.
		if _, _, findErr := root.Find(effective); findErr != nil {
			return 2
		}
		return 1
	}
	return 0
}

// resolveLogging picks the effective log level and format. An explicitly set
// flag wins; otherwise the config value (which already reflects AURA_ env
// overrides) is used, so flag > env > file.
// logConfigResult reports what config load did, in one place, so the two
// entry points that bootstrap config cannot drift apart. The resolved path is
// reduced to its basename: it embeds the operator's home directory.
func logConfigResult(ctx context.Context, logger *slog.Logger, result config.LoadResult) {
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
