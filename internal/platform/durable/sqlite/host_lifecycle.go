package sqlite

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
)

const MaxStopGrace = 10 * time.Second

func HostOptions(db *sql.DB, logger *slog.Logger, shutdownTimeout time.Duration) *Options {
	grace := max(time.Millisecond, min(MaxStopGrace, shutdownTimeout/2))
	return &Options{DB: db, Logger: logger, StopGrace: grace}
}

type RunHost interface {
	Acquire(ctx context.Context) error
	Run(ctx context.Context) error
}

type HostStage struct {
	host     RunHost
	handlers []string
	logger   *slog.Logger

	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func NewHostStage(host RunHost, handlers []string, logger *slog.Logger) *HostStage {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &HostStage{host: host, handlers: handlers, logger: logger}
}

func (d *HostStage) Name() string { return "durable_host" }

func (d *HostStage) Start(ctx context.Context) error {
	if d == nil || d.host == nil {
		return invalidArgument("durable host must not be nil")
	}
	if err := d.host.Acquire(ctx); err != nil {
		return err
	}
	hostCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	d.cancel, d.done = cancel, make(chan struct{})
	go d.run(hostCtx)
	d.logger.InfoContext(ctx, "durable host started", "component", "durable", "handlers", d.handlers)
	return nil
}

func (d *HostStage) run(ctx context.Context) {
	defer close(d.done)
	d.err = d.host.Run(ctx)
}

func (d *HostStage) Stop(ctx context.Context) error {
	if d == nil || d.cancel == nil || d.done == nil {
		return nil
	}
	d.cancel()
	select {
	case <-d.done:
		return d.err
	case <-ctx.Done():
		return fault.New(ErrorCodeStopTimeout, "durable host did not stop within the shutdown timeout")
	}
}
