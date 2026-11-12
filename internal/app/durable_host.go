package app

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/durable/sqlite"
)

const maxDurableStopGrace = 10 * time.Second

func durableHostOptions(db *sql.DB, logger *slog.Logger, runtimeCfg config.Runtime) *sqlite.Options {
	grace := max(time.Millisecond, min(maxDurableStopGrace, time.Duration(runtimeCfg.ShutdownTimeout)/2))
	return &sqlite.Options{DB: db, Logger: logger, StopGrace: grace}
}

type durableRunHost interface {
	Run(ctx context.Context) error
}

type durableHostStage struct {
	host     durableRunHost
	handlers []string
	logger   *slog.Logger

	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func (d *durableHostStage) start(ctx context.Context) error {
	hostCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	d.cancel, d.done = cancel, make(chan struct{})
	go d.run(hostCtx)
	d.logger.InfoContext(ctx, "durable host started", "component", "durable", "handlers", d.handlers)
	return nil
}

func (d *durableHostStage) run(ctx context.Context) {
	defer close(d.done)
	err := d.host.Run(ctx)
	if ctx.Err() == nil {
		d.logger.ErrorContext(ctx, "durable host stopped before shutdown", "component", "durable", "error", err)
		return
	}
	d.err = err
}

func (d *durableHostStage) stop(ctx context.Context) error {
	d.cancel()
	select {
	case <-d.done:
		return d.err
	case <-ctx.Done():
		return &Error{Code: ErrorCodeStopTimeout, Detail: "durable host did not stop within the shutdown timeout"}
	}
}
