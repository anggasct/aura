package usage

import "context"

// invocationCtxKey carries the reservation idempotency key (invocation ID and
// attempt) through a context. A retry or fallback that re-enters the budget
// wrapper with the same key reuses the existing reservation instead of
// creating a duplicate, so one logical model attempt can never produce two
// reservations or two settlement entries.
type invocationCtxKey struct{}

type invocationIdentity struct {
	id      string
	attempt int
}

// WithInvocation returns ctx annotated with the reservation idempotency key.
// The runtime or a retry/fallback adapter sets this so a replay of the same
// logical model attempt collapses onto its existing reservation.
func WithInvocation(ctx context.Context, invocationID string, attempt int) context.Context {
	return context.WithValue(ctx, invocationCtxKey{}, invocationIdentity{id: invocationID, attempt: attempt})
}

func invocationFrom(ctx context.Context) (invocationID string, attempt int, ok bool) {
	if ctx == nil {
		return "", 0, false
	}
	if v, ok := ctx.Value(invocationCtxKey{}).(invocationIdentity); ok {
		return v.id, v.attempt, true
	}
	return "", 0, false
}

// invocationCarrier is the narrow shape the budget wrapper reads from a
// runtime context to recover a stable invocation identity. Declared by the
// consumer so the package depends only on the method it calls.
type invocationCarrier interface {
	InvocationID() string
}
