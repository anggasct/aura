package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/anggasct/aura/internal/modules/broadcast"
	"github.com/anggasct/aura/internal/modules/child"
	"github.com/anggasct/aura/internal/modules/schedule"
	"github.com/anggasct/aura/internal/platform/durable/sqlite"
)

const (
	runRetentionInterval = 24 * time.Hour
	runRetentionBatch    = 500
)

type pruner interface {
	PruneEnded(ctx context.Context, req sqlite.PruneRequest) (int, error)
}

type runRetention struct {
	pruner    pruner
	retention time.Duration
	batch     int
	interval  time.Duration
	logger    *slog.Logger
}

func (r *runRetention) run(ctx context.Context) {
	r.sweep(ctx)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.sweep(ctx)
		}
	}
}

func (r *runRetention) sweep(ctx context.Context) {
	deleted, err := r.prune(ctx, time.Now().UTC().Add(-r.retention))
	if err != nil {
		if ctx.Err() == nil {
			r.logger.WarnContext(ctx, "durable run retention failed", "component", "durable", "deleted", deleted, "error", err)
		}
		return
	}
	r.logger.InfoContext(ctx, "durable run retention", "component", "durable", "deleted", deleted)
}

func (r *runRetention) prune(ctx context.Context, before time.Time) (int, error) {
	req := sqlite.PruneRequest{
		Handlers: []string{broadcast.HandlerName, schedule.HandlerName, child.HandlerName},
		Before:   before,
		Limit:    r.batch,
	}
	total := 0
	for ctx.Err() == nil {
		deleted, err := r.pruner.PruneEnded(ctx, req)
		total += deleted
		if err != nil {
			return total, err
		}
		if deleted < r.batch {
			return total, nil
		}
	}
	return total, ctx.Err()
}
