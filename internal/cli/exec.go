package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newExecCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "exec <cmd>",
		Short: "Sandboxed one-shot command execution",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), "aura exec: sandboxed execution not yet implemented")
			return nil
		},
	}
}
