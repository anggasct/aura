package cli

import (
	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/app"
)

func newDoctorCmd(gf *app.GlobalFlags) *cobra.Command {
	var checkID string
	cmd := &cobra.Command{
		Use:           "doctor [--check <id>]",
		Short:         "Run local diagnostics and print detailed findings",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return app.RunDoctor(cmd, gf, checkID)
		},
	}
	cmd.Flags().StringVar(&checkID, "check", "", "report only the named check")
	return cmd
}
