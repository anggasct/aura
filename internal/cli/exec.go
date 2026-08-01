package cli

import (
	"errors"

	"github.com/spf13/cobra"
)

func newExecCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "exec <cmd>",
		Short: "Sandboxed one-shot command execution",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return &usageError{errors.New("exec requires at least one argument")}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return errors.New("aura exec: sandboxed execution not yet implemented")
		},
	}
}
