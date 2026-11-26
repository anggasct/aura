package cli

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/app"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print build metadata",
		RunE: func(cmd *cobra.Command, _ []string) error {
			info, err := app.BuildInfo()
			if err != nil {
				return err
			}
			compiled := strings.Join(info.Capabilities, ", ")
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
`, info.Version, info.Commit, info.Date, runtime.Version(), runtime.GOOS, runtime.GOARCH, info.Profile, compiled)
			return err
		},
	}
}
