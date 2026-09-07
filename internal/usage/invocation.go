package usage

import "context"

type invocationCtxKey struct{}

type invocationIdentity struct {
	id      string
	attempt int
}

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

type invocationCarrier interface {
	InvocationID() string
}
