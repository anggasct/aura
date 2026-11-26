package sqlite

import (
	"context"
	"log/slog"
	"slices"
	"time"
)

const (
	DefaultRetentionInterval = 24 * time.Hour
	DefaultRetentionBatch    = 500
)

type Pruner interface {
	PruneEnded(ctx context.Context, req PruneRequest) (int, error)
}

type RetentionCounts struct {
	Orphaned int
	Deleted  int
}

type RetentionSweeperOptions struct {
	Pruner    Pruner
	Handlers  []string
	Orphans   func(ctx context.Context) (int, error)
	Retention time.Duration
	Batch     int
	Interval  time.Duration
	Logger    *slog.Logger
}

type RetentionSweeper struct {
	pruner    Pruner
	handlers  []string
	orphans   func(ctx context.Context) (int, error)
	retention time.Duration
	batch     int
	interval  time.Duration
	logger    *slog.Logger
}

func NewRetentionSweeper(opts *RetentionSweeperOptions) (*RetentionSweeper, error) {
	if opts == nil || opts.Pruner == nil {
		return nil, invalidArgument("retention sweeper requires a pruner")
	}
	batch := opts.Batch
	if batch <= 0 {
		batch = DefaultRetentionBatch
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultRetentionInterval
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &RetentionSweeper{
		pruner:    opts.Pruner,
		handlers:  slices.Clone(opts.Handlers),
		orphans:   opts.Orphans,
		retention: opts.Retention,
		batch:     batch,
		interval:  interval,
		logger:    logger,
	}, nil
}

func (r *RetentionSweeper) Run(ctx context.Context) {
	r.Sweep(ctx)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.Sweep(ctx)
		}
	}
}

func (r *RetentionSweeper) Sweep(ctx context.Context) {
	counts, err := r.DeleteOrphansThenPrune(ctx)
	if err != nil {
		if ctx.Err() == nil {
			r.logger.WarnContext(ctx, "durable run retention failed", "component", "durable", "deleted", counts.Deleted, "orphaned", counts.Orphaned, "error", err)
		}
		return
	}
	r.logger.InfoContext(ctx, "durable run retention", "component", "durable", "deleted", counts.Deleted, "orphaned", counts.Orphaned)
}

func (r *RetentionSweeper) DeleteOrphansThenPrune(ctx context.Context) (RetentionCounts, error) {
	var counts RetentionCounts
	var err error
	if r.orphans != nil {
		if counts.Orphaned, err = r.orphans(ctx); err != nil {
			return counts, err
		}
	}
	counts.Deleted, err = r.Prune(ctx, time.Now().UTC().Add(-r.retention))
	return counts, err
}

func (r *RetentionSweeper) Prune(ctx context.Context, before time.Time) (int, error) {
	req := PruneRequest{
		Handlers: r.handlers,
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
