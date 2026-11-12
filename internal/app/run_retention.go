package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/anggasct/aura/internal/modules/broadcast"
	"github.com/anggasct/aura/internal/modules/child"
	"github.com/anggasct/aura/internal/modules/schedule"
	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/durable/sqlite"
	"github.com/anggasct/aura/internal/platform/turn/engine"
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
	orphans   func(ctx context.Context) (int, error)
	retention time.Duration
	batch     int
	interval  time.Duration
	logger    *slog.Logger
}

type runRetentionCounts struct {
	orphaned int
	deleted  int
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
	counts, err := r.deleteOrphansThenPrune(ctx)
	if err != nil {
		if ctx.Err() == nil {
			r.logger.WarnContext(ctx, "durable run retention failed", "component", "durable", "deleted", counts.deleted, "orphaned", counts.orphaned, "error", err)
		}
		return
	}
	r.logger.InfoContext(ctx, "durable run retention", "component", "durable", "deleted", counts.deleted, "orphaned", counts.orphaned)
}

func (r *runRetention) deleteOrphansThenPrune(ctx context.Context) (runRetentionCounts, error) {
	var counts runRetentionCounts
	var err error
	if counts.orphaned, err = r.orphans(ctx); err != nil {
		return counts, err
	}
	counts.deleted, err = r.prune(ctx, time.Now().UTC().Add(-r.retention))
	return counts, err
}

func (r *runRetention) prune(ctx context.Context, before time.Time) (int, error) {
	req := sqlite.PruneRequest{
		Handlers: []string{broadcast.HandlerName, schedule.HandlerName, child.HandlerName, workflow.HandlerName, runtimeengine.HandlerName},
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
