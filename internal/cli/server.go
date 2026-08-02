package cli

import (
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/logging"
	"github.com/anggasct/aura/internal/model"
	"github.com/anggasct/aura/internal/server"
)

func newServerCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "server",
		Short: "Start the daemon with all listeners",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			result, err := config.Load(gf.configPath)
			if err != nil {
				return err
			}
			cfg := result.Config
			level, format := resolveLogging(cmd, cfg)
			logger, err := logging.Setup(level, format, os.Stderr)
			if err != nil {
				return err
			}
			logConfigResult(ctx, logger, result)
			if _, err := model.BuildRouter(logger, cfg.Models); err != nil {
				return err
			}
			if err := model.RegisterAdapters(logger, cfg.Models); err != nil {
				return err
			}
			logger.InfoContext(ctx, "starting server",
				"component", "server",
				"host", cfg.Server.Host,
				"port", cfg.Server.Port,
			)
			srv := server.New(server.Options{
				Logger:          logger,
				ShutdownTimeout: time.Duration(cfg.Runtime.ShutdownTimeout),
			})
			return srv.Run(ctx)
		},
	}
}
