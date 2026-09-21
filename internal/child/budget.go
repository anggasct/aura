package child

import (
	stdcontext "context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

const defaultReservationTTL = time.Hour

type usageTotals struct {
	tokens int64
	cost   int64
}

type reservationState struct {
	id           string
	invocationID string
	ownerID      string
	maxTokens    int64
	maxCost      int64
	usedTokens   int64
	usedCost     int64
	released     bool
}

type Ledger struct {
	mu           sync.Mutex
	clock        Clock
	ttl          time.Duration
	reservations map[string]*reservationState
	parents      map[string]*usageTotals
	owners       map[string]*usageTotals
}

func NewLedger(clock Clock, reservationTTL time.Duration) *Ledger {
	if clock == nil {
		clock = systemClock{}
	}
	if reservationTTL <= 0 {
		reservationTTL = defaultReservationTTL
	}
	return &Ledger{
		clock:        clock,
		ttl:          reservationTTL,
		reservations: map[string]*reservationState{},
		parents:      map[string]*usageTotals{},
		owners:       map[string]*usageTotals{},
	}
}

func randomReservationID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("child: generate reservation id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func (l *Ledger) Reserve(ctx stdcontext.Context, invocationID, ownerID string, maxTokens, maxCost int64) (BudgetReservation, error) {
	if ctx == nil {
		return BudgetReservation{}, Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return BudgetReservation{}, err
	}
	if invocationID == "" {
		return BudgetReservation{}, Errorf(ErrorCodeInvalidArgument, "invocation must not be empty")
	}
	if maxTokens < 0 || maxCost < 0 {
		return BudgetReservation{}, Errorf(ErrorCodeInvalidArgument, "child budget must not be negative")
	}
	id, err := randomReservationID()
	if err != nil {
		return BudgetReservation{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	expiresAt := l.clock.Now().UTC().Add(l.ttl)
	l.reservations[id] = &reservationState{
		id: id, invocationID: invocationID, ownerID: ownerID,
		maxTokens: maxTokens, maxCost: maxCost,
	}
	return BudgetReservation{ID: id, ExpiresAt: expiresAt}, nil
}

func (l *Ledger) Charge(ctx stdcontext.Context, reservationID string, tokens, cost int64) error {
	if ctx == nil {
		return Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if reservationID == "" {
		return Errorf(ErrorCodeInvalidArgument, "reservation must not be empty")
	}
	if tokens < 0 || cost < 0 {
		return Errorf(ErrorCodeInvalidArgument, "child usage must not be negative")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	state, ok := l.reservations[reservationID]
	if !ok {
		return Errorf(ErrorCodeChildNotFound, "child reservation is not found")
	}
	if state.released {
		return Errorf(ErrorCodeChildConflict, "child reservation is already released")
	}
	if state.usedTokens+tokens > state.maxTokens || state.usedCost+cost > state.maxCost {
		return Errorf(ErrorCodeBudgetExceeded, "child budget is exceeded")
	}
	state.usedTokens += tokens
	state.usedCost += cost
	parent := l.parents[state.invocationID]
	if parent == nil {
		parent = &usageTotals{}
		l.parents[state.invocationID] = parent
	}
	parent.tokens += tokens
	parent.cost += cost
	if state.ownerID != "" {
		owner := l.owners[state.ownerID]
		if owner == nil {
			owner = &usageTotals{}
			l.owners[state.ownerID] = owner
		}
		owner.tokens += tokens
		owner.cost += cost
	}
	return nil
}

func (l *Ledger) Release(ctx stdcontext.Context, reservationID string) error {
	if ctx == nil {
		return Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if reservationID == "" {
		return Errorf(ErrorCodeInvalidArgument, "reservation must not be empty")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	state, ok := l.reservations[reservationID]
	if !ok {
		return Errorf(ErrorCodeChildNotFound, "child reservation is not found")
	}
	state.released = true
	return nil
}

func (l *Ledger) ParentUsage(invocationID string) (tokens, cost int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if totals := l.parents[invocationID]; totals != nil {
		return totals.tokens, totals.cost
	}
	return 0, 0
}

func (l *Ledger) OwnerUsage(ownerID string) (tokens, cost int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if totals := l.owners[ownerID]; totals != nil {
		return totals.tokens, totals.cost
	}
	return 0, 0
}
