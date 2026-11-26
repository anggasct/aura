package cli

import (
	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/app"
)

func newStatusCmd(gf *app.GlobalFlags) *cobra.Command {
	var flags app.StatusCommandFlags
	cmd := &cobra.Command{
		Use:           "status",
		Short:         "Report system and sandbox health",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return app.RunStatus(cmd, gf, flags)
		},
	}
	cmd.Flags().BoolVar(&flags.AsJSON, "json", false, "emit machine-readable output")
	cmd.Flags().BoolVar(&flags.Offline, "offline", false, "skip the live process probe and use local stores only")
	cmd.Flags().StringVar(&flags.Probe, "probe", "", "query the running process's live or ready probe and exit 0 only when it passes (container health check)")
	return cmd
}
