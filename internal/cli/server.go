package cli

import (
	"log/slog"
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
			result, err := config.Load(gf.configPath)
			if err != nil {
				return err
			}
			cfg := result.Config
			level, format := resolveLogging(cmd, cfg)
			if err := logging.Setup(level, format, os.Stderr); err != nil {
				return err
			}
			if result.DefaultGenerated {
				slog.Info("generating default config", "component", "config", "path", result.Path)
			}
			if _, err := model.BuildRouter(cfg.Models); err != nil {
				return err
			}
			if err := model.RegisterAdapters(cfg.Models); err != nil {
				return err
			}
			slog.Info("starting server",
				"component", "server",
				"host", cfg.Server.Host,
				"port", cfg.Server.Port,
			)
			srv := server.New(server.Options{
				ShutdownTimeout: time.Duration(cfg.Server.ShutdownTimeout),
			})
			return srv.Run(cmd.Context())
		},
	}
}
