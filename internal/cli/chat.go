package cli

import (
	"errors"

	"github.com/spf13/cobra"
)

func newChatCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "chat",
		Short: "Interactive terminal console",
		RunE: func(cmd *cobra.Command, args []string) error {
			return errors.New("aura chat: interactive console not yet implemented")
		},
	}
}
