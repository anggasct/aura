package shellexec

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/command"
)

func Commands(wrapUsage func(error) error) []*cobra.Command {
	if wrapUsage == nil {
		wrapUsage = command.NewUsageError
	}
	return []*cobra.Command{
		{
			Use:   "exec <cmd>",
			Short: "Sandboxed one-shot command execution",
			Args: func(_ *cobra.Command, args []string) error {
				if len(args) == 0 {
					return wrapUsage(errors.New("exec requires at least one argument"))
				}
				return nil
			},
			RunE: func(_ *cobra.Command, _ []string) error {
				return errors.New("aura exec: sandboxed execution not yet implemented")
			},
		},
	}
}
