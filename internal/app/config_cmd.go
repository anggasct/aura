package app

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/logging"
)

func newConfigCmd(gf *globalFlags) *cobra.Command {
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
		RunE: func(cmd *cobra.Command, args []string) error {
			schema, err := config.Schema()
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(schema)
			return err
		},
	}
}

func newConfigCheckCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Load and validate the configuration without starting anything",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := config.Load(gf.configPath)
			if err != nil {
				return err
			}
			level, format := resolveLogging(cmd, result.Config)
			logger, err := logging.Setup(level, format, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			logConfigResult(cmd.Context(), logger, &result)
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "config ok: %s\n", filepath.Base(result.Path))
			return err
		},
	}
}
