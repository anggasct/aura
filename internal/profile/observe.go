package profile

import (
	stdcontext "context"
	"time"
)

const (
	ObserveExtraction = "extraction"
	ObserveExpiry     = "expiry"
	ObserveContext    = "context"
)

const (
	ResultCompleted   = "completed"
	ResultFailed      = "failed"
	ResultParseFailed = "parse_failed"
	ResultDropped     = "dropped"
)

type Observation struct {
	Kind     string
	Result   string
	QueueAge time.Duration
	Lag      time.Duration
	Accepted int
	Screened int
	Skipped  int
	Facts    int
	Tokens   int
}

type Observer func(ctx stdcontext.Context, observation *Observation)

func observeWith(ctx stdcontext.Context, observer Observer, observation *Observation) {
	if observer == nil {
		return
	}
	observer(ctx, observation)
}
