package cli

import (
	"context"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/logging"
	"github.com/anggasct/aura/internal/model"
	auraruntime "github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/server"
	"github.com/anggasct/aura/internal/store"
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
			logConfigResult(ctx, logger, &result)
			if _, err := model.BuildRouter(logger, cfg.Models); err != nil {
				return err
			}
			if err := model.RegisterAdapters(logger, cfg.Models); err != nil {
				return err
			}
			db, err := openStorage(ctx, cfg)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			builtin, err := newBuiltinToolExecutor(cfg, db, logger)
			if err != nil {
				return err
			}
			modelDefinition := cfg.Models.Definitions["primary"]
			if modelDefinition.Model == "" {
				return &config.Error{Code: config.ErrorCodeConfigInvalid, Detail: "models.definitions.primary.model is required for the server runtime"}
			}
			sessions := store.NewSessionService(db)
			events := store.NewEventStore(db)
			adkExecutor, err := auraruntime.NewADKExecutor(
				"aura", modelDefinition.Model, sessions, events, builtin, nil, logger,
				auraruntime.WithBuiltinToolExecutor(builtin),
			)
			if err != nil {
				return err
			}
			runtimeEngine, err := auraruntime.NewEngine(auraruntime.Config{
				MaxActiveTurns:  cfg.Runtime.MaxActiveTurns,
				MaxPendingTurns: cfg.Runtime.MaxPendingTurns,
				TurnTimeout:     time.Duration(cfg.Runtime.TurnTimeout),
				ShutdownTimeout: time.Duration(cfg.Runtime.ShutdownTimeout),
			}, events, store.NewDedupeStore(db), adkExecutor, logger)
			if err != nil {
				return err
			}
			host, err := auraruntime.NewHost(runtimeEngine, nil, logger)
			if err != nil {
				return err
			}
			if err := host.Start(ctx); err != nil {
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
			runErr := srv.Run(ctx)
			shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.Runtime.ShutdownTimeout))
			defer cancel()
			if err := host.Shutdown(shutdownCtx); runErr == nil {
				runErr = err
			}
			return runErr
		},
	}
}
