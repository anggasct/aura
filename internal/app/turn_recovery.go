package app

import (
	"context"
	"log/slog"
	"time"
)

const turnRecoveryInterval = time.Minute

type turnRecoverer interface {
	RecoverTurns(ctx context.Context) error
}

type turnRecovery struct {
	recoverer turnRecoverer
	logger    *slog.Logger
	interval  time.Duration
}

func (r *turnRecovery) start(ctx context.Context) error {
	r.pass(ctx)
	return nil
}

func (r *turnRecovery) run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.pass(ctx)
		}
	}
}

func (r *turnRecovery) pass(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	if err := r.recoverer.RecoverTurns(ctx); err != nil && ctx.Err() == nil {
		r.logger.WarnContext(ctx, "turn recovery failed", "component", "durable", "error", err)
	}
}
