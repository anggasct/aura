package cli

import (
	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/app"
)

func newChatCmd(gf *app.GlobalFlags) *cobra.Command {
	var plain bool
	cmd := &cobra.Command{
		Use:   "chat [--plain] [--session <id>] [--persona <id>]",
		Short: "Interactive terminal console",
		RunE: func(cmd *cobra.Command, _ []string) error {
			sessionID, _ := cmd.Flags().GetString("session")
			persona, _ := cmd.Flags().GetString("persona")
			return app.RunChat(cmd, gf, app.ChatCommandFlags{
				Plain:     plain,
				SessionID: sessionID,
				Persona:   persona,
			})
		},
	}
	cmd.Flags().BoolVar(&plain, "plain", false, "force non-TTY plain output")
	cmd.Flags().String("session", "", "resume an existing session")
	cmd.Flags().String("persona", "", "run turns as this persona (default main; a resumed session keeps its own)")
	return cmd
}
