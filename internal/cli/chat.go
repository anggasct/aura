package cli

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/logging"
)

func newChatCmd(gf *globalFlags) *cobra.Command {
	var plain bool
	cmd := &cobra.Command{
		Use:   "chat [--plain] [--session <id>]",
		Short: "Interactive terminal console",
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionID, _ := cmd.Flags().GetString("session")
			ctx := cmd.Context()
			result, err := config.Load(gf.configPath)
			if err != nil {
				return err
			}
			cfg := result.Config
			level, format := resolveLogging(cmd, cfg)
			logger, err := logging.Setup(level, format, os.Stderr)
			if err != nil {
				return err
			}
			logConfigResult(ctx, logger, &result)
			if result.CapabilityStateError != nil {
				return result.CapabilityStateError
			}
			return runChat(ctx, cfg, logger, os.Stdin, os.Stdout, os.Stderr, sessionID, chatPresentation{
				plain:   plain,
				noColor: os.Getenv("NO_COLOR") != "",
			})
		},
	}
	cmd.Flags().BoolVar(&plain, "plain", false, "force non-TTY plain output")
	cmd.Flags().String("session", "", "resume an existing session")
	return cmd
}
