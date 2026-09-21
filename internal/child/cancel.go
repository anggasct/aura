package child

import (
	stdcontext "context"
	"time"
)

type Canceller interface {
	CancelRun(ctx stdcontext.Context, durableKey string) error
}

type CancelService struct {
	registry CancelRegistry
	runs     Canceller
}

type CancelRegistry interface {
	Get(ctx stdcontext.Context, id string) (Spawn, bool, error)
	RunState(ctx stdcontext.Context, id string) (string, error)
	SetChildState(ctx stdcontext.Context, id, state string, now time.Time) error
}

func NewCancelService(registry CancelRegistry, runs Canceller) (*CancelService, error) {
	if registry == nil {
		return nil, errNilArgument("registry")
	}
	if runs == nil {
		return nil, errNilArgument("runs")
	}
	return &CancelService{registry: registry, runs: runs}, nil
}

func (s *CancelService) Cancel(ctx stdcontext.Context, id string, now time.Time) (string, error) {
	if ctx == nil {
		return "", Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if now.IsZero() {
		return "", Errorf(ErrorCodeInvalidArgument, "timestamp must not be zero")
	}
	spawn, found, err := s.registry.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if !found {
		return "", Errorf(ErrorCodeChildNotFound, "child is not found")
	}
	state, err := s.registry.RunState(ctx, id)
	if err != nil {
		return "", err
	}
	switch state {
	case StatusSucceeded, StatusFailed, StatusCancelled, StatusDeadline, StatusInterrupted:
		return state, nil
	}
	if err := s.runs.CancelRun(ctx, spawn.DurableKey); err != nil {
		return "", err
	}
	if err := s.registry.SetChildState(ctx, id, StatusCancelled, now); err != nil {
		return "", err
	}
	return StatusCancelled, nil
}
