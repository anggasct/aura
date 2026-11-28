package turn

import (
	"context"
	"time"
)

type startedAtContextKey struct{}

func WithStartedAt(ctx context.Context, startedAt time.Time) context.Context {
	if startedAt.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, startedAtContextKey{}, startedAt.UTC())
}

func StartedAtFrom(ctx context.Context) (time.Time, bool) {
	startedAt, ok := ctx.Value(startedAtContextKey{}).(time.Time)
	if !ok || startedAt.IsZero() {
		return time.Time{}, false
	}
	return startedAt, true
}

const (
	ContextOrderRecall    = 100
	ContextOrderProfile   = 200
	ContextOrderSkills    = 300
	ContextOrderKnowledge = 400
)

type ContextPlacement int

const (
	ContextPlacementEphemeral ContextPlacement = iota
	ContextPlacementUserPart
)

type ContextBlock struct {
	Order     int
	Placement ContextPlacement
	Content   string
	Caveat    string
	Events    []Event
	Taint     *TaintMark
}

type ContextProvider interface {
	TurnContext(ctx context.Context, req *TurnRequest) ([]ContextBlock, error)
	Required(req *TurnRequest) bool
}
