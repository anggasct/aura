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
			compiled := strings.Join(build.CompiledCapabilities(), ", ")
			if compiled == "" {
				compiled = "none"
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), `aura %s
  commit:   %s
  built:    %s
  go:       %s
  platform: %s/%s
  profile:  %s
  capabilities: %s
`, version, commit, date, runtime.Version(), runtime.GOOS, runtime.GOARCH, build.Profile(), compiled)
			return err
		},
	}
}
