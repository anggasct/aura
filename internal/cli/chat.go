package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newChatCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "chat",
		Short: "Interactive terminal console",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), "aura chat: interactive console not yet implemented")
			return nil
		},
	}
}
