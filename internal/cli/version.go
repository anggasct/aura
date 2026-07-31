package cli

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/capability"
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
			build, err := capability.CurrentBuild()
			if err != nil {
				return fmt.Errorf("capability: %w", err)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "aura %s\n", version)
			fmt.Fprintf(out, "  commit:   %s\n", commit)
			fmt.Fprintf(out, "  built:    %s\n", date)
			fmt.Fprintf(out, "  go:       %s\n", runtime.Version())
			fmt.Fprintf(out, "  platform: %s/%s\n", runtime.GOOS, runtime.GOARCH)
			fmt.Fprintf(out, "  profile:  %s\n", build.Profile())
			compiled := strings.Join(build.CompiledCapabilities(), ", ")
			if compiled == "" {
				compiled = "none"
			}
			fmt.Fprintf(out, "  capabilities: %s\n", compiled)
			return nil
		},
	}
}
