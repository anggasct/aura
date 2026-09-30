package child

import (
	stdcontext "context"
	"errors"
	"time"

	"github.com/anggasct/aura/internal/durable"
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

type activeLister interface {
	ActiveForParent(ctx stdcontext.Context, parentSessionID string) ([]Spawn, error)
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
	if !spawn.Deadline.IsZero() && !now.UTC().Before(spawn.Deadline.UTC()) {
		if err := s.runs.CancelRun(ctx, spawn.DurableKey); err != nil {
			if !errors.Is(err, durable.ErrUnknownRun) {
				return "", err
			}
		}
		if err := s.registry.SetChildState(ctx, id, StatusDeadline, now); err != nil {
			return "", err
		}
		return StatusDeadline, nil
	}
	if err := s.runs.CancelRun(ctx, spawn.DurableKey); err != nil {
		if !errors.Is(err, durable.ErrUnknownRun) {
			return "", err
		}
	}
	if err := s.registry.SetChildState(ctx, id, StatusCancelled, now); err != nil {
		return "", err
	}
	return StatusCancelled, nil
}

func (s *CancelService) CancelTree(ctx stdcontext.Context, parentSessionID string, now time.Time, leaveBackground bool) (map[string]string, error) {
	if ctx == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if now.IsZero() {
		return nil, Errorf(ErrorCodeInvalidArgument, "timestamp must not be zero")
	}
	if parentSessionID == "" {
		return nil, Errorf(ErrorCodeInvalidArgument, "parent session must not be empty")
	}
	lister, ok := s.registry.(activeLister)
	if !ok {
		return nil, Errorf(ErrorCodeInvalidArgument, "registry does not support parent cancellation")
	}
	drainCtx := ctx
	if err := ctx.Err(); err != nil {
		drainCtx = stdcontext.WithoutCancel(ctx)
	}
	active, err := lister.ActiveForParent(drainCtx, parentSessionID)
	if err != nil {
		return nil, err
	}
	if len(active) > 4 {
		return nil, Errorf(ErrorCodeRuntimeOverloaded, "parent has too many active children")
	}
	results := make(map[string]string, len(active))
	var firstErr error
	for i := range active {
		spawn := &active[i]
		if leaveBackground && spawn.Background {
			results[spawn.ID] = StatusRunning
			continue
		}
		state, err := s.Cancel(drainCtx, spawn.ID, now)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		results[spawn.ID] = state
	}
	if err := ctx.Err(); err != nil {
		return results, Errorf(ErrorCodeTurnCancelled, "parent cancellation was interrupted")
	}
	if firstErr != nil {
		return results, firstErr
	}
	return results, nil
}
