package memory

import (
	"context"
	"time"
)

const (
	OutcomeOK    = "ok"
	OutcomeEmpty = "empty"
)

const (
	maxObservedScores  = 10
	maxObservedSources = 10
	maxEvidenceRunes   = 4000
)

type Observation struct {
	Outcome        string
	Summarized     bool
	Documents      int
	Screened       int
	Scores         []float64
	SourceIDs      []string
	ModelProtocol  string
	ModelName      string
	SummaryVersion string
	Duration       time.Duration
}

type Observer func(ctx context.Context, observation *Observation)

func (s *Service) observe(ctx context.Context, observation *Observation) {
	if s.observer == nil || observation == nil {
		return
	}
	s.observer(ctx, observation)
}
