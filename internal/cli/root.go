package cli

import (
	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/app"
)

func NewRootCommand() *cobra.Command {
	gf := &app.GlobalFlags{}
	root := &cobra.Command{
		Use:           "aura",
		Short:         "Self-hosted personal AI agent",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	pf := root.PersistentFlags()
	pf.StringVar(&gf.ConfigPath, "config", "", "path to config.yaml")
	pf.StringVar(&gf.LogLevel, "log-level", "info", "log level (debug, info, warn, error)")
	pf.StringVar(&gf.LogFormat, "log-format", "text", "log format (text, json)")

	root.AddCommand(
		newVersionCmd(),
		newServerCmd(gf),
		newChatCmd(gf),
		newStatusCmd(gf),
		newDoctorCmd(gf),
		newConfigCmd(gf),
	)
	app.MountCommands(root, gf)
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return app.NewUsageError(err)
	})
	return root
}
