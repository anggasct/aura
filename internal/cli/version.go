package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print build metadata",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "aura %s\n", version)
			fmt.Fprintf(out, "  commit:   %s\n", commit)
			fmt.Fprintf(out, "  built:    %s\n", date)
			fmt.Fprintf(out, "  go:       %s\n", runtime.Version())
			fmt.Fprintf(out, "  platform: %s/%s\n", runtime.GOOS, runtime.GOARCH)
			return nil
		},
	}
}
