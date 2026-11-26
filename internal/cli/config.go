package cli

import (
	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/app"
)

func newConfigCmd(gf *app.GlobalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect and validate the configuration",
	}
	cmd.AddCommand(newConfigSchemaCmd(), newConfigCheckCmd(gf))
	return cmd
}

func newConfigSchemaCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "schema",
		Short: "Print the JSON Schema of the configuration file",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return app.ConfigSchema(cmd.OutOrStdout())
		},
	}
}

func newConfigCheckCmd(gf *app.GlobalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Load and validate the configuration without starting anything",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return app.ConfigCheck(cmd, gf)
		},
	}
}
