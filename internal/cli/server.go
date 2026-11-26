package cli

import (
	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/app"
)

func newServerCmd(gf *app.GlobalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "server",
		Short: "Start the daemon with all listeners",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return app.RunServer(cmd, gf)
		},
	}
}
