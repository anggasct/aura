package context

import (
	stdcontext "context"
	"time"
)

type ObservationOutcome string

const (
	OutcomeProduced ObservationOutcome = "produced"
	OutcomeCacheHit ObservationOutcome = "cache_hit"
	OutcomeRejected ObservationOutcome = "rejected"
)

type Observation struct {
	Outcome       ObservationOutcome
	SessionID     string
	StartSequence uint64
	EndSequence   uint64
	EventCount    int
	SourceTokens  int
	SummaryTokens int
	Accounting    AccountingClass
	CacheHit      bool
	ModelName     string
	PromptVersion string
	Duration      time.Duration
}

type Observer func(ctx stdcontext.Context, observation *Observation)

func (s *Service) observe(ctx stdcontext.Context, observation *Observation) {
	if s.observer == nil || observation == nil {
		return
	}
	s.observer(ctx, observation)
}
