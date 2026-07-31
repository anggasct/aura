package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/config"
)

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

	root.AddCommand(newVersionCmd(), newServerCmd(gf), newChatCmd(), newExecCmd())
	return root
}

func Execute() int {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "aura:", err)
		return 1
	}
	return 0
}

// resolveLogging picks the effective log level and format. An explicitly set
// flag wins; otherwise the config value (which already reflects AURA_ env
// overrides) is used, so flag > env > file.
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
