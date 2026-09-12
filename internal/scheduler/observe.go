package scheduler

import (
	"context"
	"time"
)

const (
	ResultFired   = "fired"
	ResultSettled = "settled"
	ResultRetried = "retried"
)

type Observation struct {
	State  string
	Result string
	Lag    time.Duration
	Age    time.Duration
}

type Observer func(ctx context.Context, observation *Observation)

func (r *Runner) observe(ctx context.Context, observation *Observation) {
	if r.observer == nil {
		return
	}
	r.observer(ctx, observation)
}
