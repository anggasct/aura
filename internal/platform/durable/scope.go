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
	models uint64
	evs    uint64
	clocks uint64
}

func NewTurnScope(inv Invocation) *TurnScope {
	if inv == nil {
		return nil
	}
	return &TurnScope{inv: inv}
}

func NewTurnScopeWithin(inv Invocation, prefix string) *TurnScope {
	if inv == nil {
		return nil
	}
	return &TurnScope{inv: &prefixedInvocation{Invocation: inv, prefix: prefix}}
}

type prefixedInvocation struct {
	Invocation
	prefix string
}

func (p *prefixedInvocation) RunAction(ctx context.Context, key string, fn func(ctx context.Context) ([]byte, error)) ([]byte, error) {
	return p.Invocation.RunAction(ctx, p.prefix+key, fn)
}

func (p *prefixedInvocation) ConcurrentActions() bool {
	return ConcurrentActions(p.Invocation)
}

func (s *TurnScope) Invocation() Invocation {
	return s.inv
}

func (s *TurnScope) NextModelCall() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.models++
	return fmt.Sprintf("model-%d", s.models)
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
