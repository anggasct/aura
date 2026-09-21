package child

import (
	stdcontext "context"
	"errors"
	"fmt"
	"time"
)

type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

type BudgetReservation struct {
	ID        string
	ExpiresAt time.Time
}

type BudgetLedger interface {
	Reserve(ctx stdcontext.Context, invocationID, ownerID string, maxTokens, maxCost int64) (BudgetReservation, error)
	Charge(ctx stdcontext.Context, reservationID string, tokens, cost int64) error
	Release(ctx stdcontext.Context, reservationID string) error
}

type Starter struct {
	service  *Service
	launcher Launcher
	ledger   BudgetLedger
	clock    Clock
}

func NewStarter(service *Service, launcher Launcher, ledger BudgetLedger, clocks Clock) (*Starter, error) {
	if service == nil {
		return nil, errNilArgument("service")
	}
	if launcher == nil {
		return nil, errNilArgument("launcher")
	}
	if ledger == nil {
		return nil, errNilArgument("ledger")
	}
	if clocks == nil {
		clocks = systemClock{}
	}
	return &Starter{service: service, launcher: launcher, ledger: ledger, clock: clocks}, nil
}

func (s *Starter) StartChild(ctx stdcontext.Context, spec *Spec, now time.Time) (Spawn, bool, error) {
	if ctx == nil {
		return Spawn{}, false, Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return Spawn{}, false, err
	}
	if spec == nil {
		return Spawn{}, false, errNilArgument("spec")
	}
	if now.IsZero() {
		return Spawn{}, false, Errorf(ErrorCodeInvalidArgument, "timestamp must not be zero")
	}
	if err := checkSpec(spec); err != nil {
		return Spawn{}, false, err
	}
	reservation, err := s.ledger.Reserve(ctx, spec.ParentInvocation, spec.OwnerID, spec.Budget.MaxTokens, spec.Budget.MaxCost)
	if err != nil {
		return Spawn{}, false, fmt.Errorf("child: reserve budget: %w", err)
	}
	spawn, created, err := s.service.SpawnChild(ctx, spec, now)
	if err != nil {
		if releaseErr := s.ledger.Release(ctx, reservation.ID); releaseErr != nil {
			return Spawn{}, false, errors.Join(err, fmt.Errorf("child: release budget: %w", releaseErr))
		}
		return Spawn{}, false, err
	}
	if !created {
		if releaseErr := s.ledger.Release(ctx, reservation.ID); releaseErr != nil {
			return Spawn{}, false, fmt.Errorf("child: release budget: %w", releaseErr)
		}
		return spawn, false, nil
	}
	payload, err := encodeStartPayload(&spawn, reservation)
	if err != nil {
		if releaseErr := s.ledger.Release(ctx, reservation.ID); releaseErr != nil {
			return Spawn{}, false, errors.Join(err, fmt.Errorf("child: release budget: %w", releaseErr))
		}
		return Spawn{}, false, err
	}
	key, err := s.launcher.Start(ctx, StartRequest{Handler: HandlerName, DurableKey: spawn.DurableKey, Payload: payload})
	if err != nil {
		if releaseErr := s.ledger.Release(ctx, reservation.ID); releaseErr != nil {
			return Spawn{}, false, errors.Join(fmt.Errorf("child: start durable invocation: %w", err), fmt.Errorf("child: release budget: %w", releaseErr))
		}
		return Spawn{}, false, fmt.Errorf("child: start durable invocation: %w", err)
	}
	if key == "" || key != spawn.DurableKey {
		if releaseErr := s.ledger.Release(ctx, reservation.ID); releaseErr != nil {
			return Spawn{}, false, errors.Join(Errorf(ErrorCodeChildInvalid, "child durable key mismatch"), fmt.Errorf("child: release budget: %w", releaseErr))
		}
		return Spawn{}, false, Errorf(ErrorCodeChildInvalid, "child durable key mismatch")
	}
	return spawn, true, nil
}
