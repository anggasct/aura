package broadcast

import (
	"context"
	"time"
)

const (
	ResultSubmitted   = "submitted"
	ResultDelivered   = "delivered"
	ResultUnknown     = "unknown"
	ResultFailed      = "failed"
	ResultRetried     = "retried"
	ResultFallback    = "fallback"
	ResultDigested    = "digested"
	ResultRateDelayed = "rate_delayed"
)

type Observation struct {
	Priority    string
	State       string
	Result      string
	Attempts    int
	Age         time.Duration
	RateDelay   time.Duration
	DigestCount int
}

type Observer func(ctx context.Context, observation *Observation)

func (b *Broadcaster) observe(ctx context.Context, observation *Observation) {
	if b.observer == nil {
		return
	}
	b.observer(ctx, observation)
}

func (r *Runner) observe(ctx context.Context, observation *Observation) {
	if r.observer == nil {
		return
	}
	r.observer(ctx, observation)
}
