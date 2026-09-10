package durable

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

type TurnScope struct {
	mu     sync.Mutex
	inv    Invocation
	ops    uint64
	evs    uint64
	clocks uint64
}

func NewTurnScope(inv Invocation) *TurnScope {
	if inv == nil {
		return nil
	}
	return &TurnScope{inv: inv}
}

func (s *TurnScope) Invocation() Invocation {
	return s.inv
}

func (s *TurnScope) NextOp() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ops++
	return fmt.Sprintf("op-%d", s.ops)
}

func (s *TurnScope) NextEventID(turnID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evs++
	return fmt.Sprintf("%s-ev-%d", turnID, s.evs)
}

func (s *TurnScope) NextClock() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clocks++
	return fmt.Sprintf("clock-%d", s.clocks)
}

type turnScopeKey struct{}

func WithTurnScope(ctx context.Context, scope *TurnScope) context.Context {
	if scope == nil {
		return ctx
	}
	return context.WithValue(ctx, turnScopeKey{}, scope)
}

func TurnScopeFrom(ctx context.Context) (*TurnScope, bool) {
	scope, ok := ctx.Value(turnScopeKey{}).(*TurnScope)
	if !ok || scope == nil {
		return nil, false
	}
	return scope, true
}

func RecoveredPanic() error {
	r := recover()
	if r == nil {
		return nil
	}
	if err, ok := r.(error); ok && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return err
	}
	panic(r)
}
