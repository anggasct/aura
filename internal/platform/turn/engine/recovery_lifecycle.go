package runtimeengine

import (
	"context"
	"log/slog"
	"time"
)

const DefaultRecoveryInterval = time.Minute

type TurnRecoverer interface {
	RecoverTurns(ctx context.Context) error
}

type Recovery struct {
	recoverer TurnRecoverer
	logger    *slog.Logger
	interval  time.Duration
}

func NewRecovery(recoverer TurnRecoverer, logger *slog.Logger, interval time.Duration) *Recovery {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if interval <= 0 {
		interval = DefaultRecoveryInterval
	}
	return &Recovery{recoverer: recoverer, logger: logger, interval: interval}
}

func (r *Recovery) Name() string { return "turn_recovery" }

func (r *Recovery) Start(ctx context.Context) error {
	r.Pass(ctx)
	return nil
}

func (r *Recovery) Stop(context.Context) error { return nil }

func (r *Recovery) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.Pass(ctx)
		}
	}
}

func (r *Recovery) Pass(ctx context.Context) {
	if r == nil || r.recoverer == nil || ctx.Err() != nil {
		return
	}
	if err := r.recoverer.RecoverTurns(ctx); err != nil && ctx.Err() == nil {
		r.logger.WarnContext(ctx, "turn recovery failed", "component", "durable", "error", err)
	}
}
