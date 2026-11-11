package app

import (
	"context"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/capability"
)

func newChatCmd(gf *globalFlags) *cobra.Command {
	var plain bool
	cmd := &cobra.Command{
		Use:   "chat [--plain] [--session <id>] [--persona <id>]",
		Short: "Interactive terminal console",
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionID, _ := cmd.Flags().GetString("session")
			persona, _ := cmd.Flags().GetString("persona")
			ctx := cmd.Context()
			result, err := config.Load(gf.configPath)
			if err != nil {
				return err
			}
			level, format := resolveLogging(cmd, result.Config)
			app, err := Build(ctx, &Options{
				Surface:   SurfaceChat,
				Config:    &result,
				LogLevel:  level,
				LogFormat: format,
				LogOutput: os.Stderr,
				Chat: ChatOptions{
					In:         os.Stdin,
					Out:        os.Stdout,
					Diag:       os.Stderr,
					SessionID:  sessionID,
					Persona:    persona,
					Plain:      plain,
					NoColor:    os.Getenv("NO_COLOR") != "",
					Interrupts: interruptsFromContext(ctx),
				},
			})
			if err != nil {
				return err
			}
			return runAndClose(ctx, app)
		},
	}
	cmd.Flags().BoolVar(&plain, "plain", false, "force non-TTY plain output")
	cmd.Flags().String("session", "", "resume an existing session")
	cmd.Flags().String("persona", "", "run turns as this persona (default main; a resumed session keeps its own)")
	return cmd
}

func chatCapabilityGate(ctx context.Context, logger *slog.Logger, result *config.LoadResult) error {
	if result.CapabilityStateError == nil {
		return nil
	}
	if !capability.IsMissingDependency(result.CapabilityStateError) {
		return result.CapabilityStateError
	}
	logger.WarnContext(ctx, "capability dependency is missing; tools that need it are not offered",
		"component", "chat",
		"error", result.CapabilityStateError,
	)
	return nil
}
