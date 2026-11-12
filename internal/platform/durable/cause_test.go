package durable

import (
	"context"
	"testing"
	"time"
)

func cancelledWith(t *testing.T, cause error) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(cause)
	return ctx
}

func timeoutChild(parent context.Context, t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func TestPreemptedReportsAStopOrALostRunOnly(t *testing.T) {
	cases := []struct {
		name string
		ctx  func(t *testing.T) context.Context
		want bool
	}{
		{name: "live context", want: false, ctx: func(t *testing.T) context.Context { return t.Context() }},
		{name: "host stopping", want: true, ctx: func(t *testing.T) context.Context { return cancelledWith(t, ErrHostStopping) }},
		{name: "ownership lost", want: true, ctx: func(t *testing.T) context.Context { return cancelledWith(t, ErrOwnershipLost) }},
		{name: "run cancelled", want: false, ctx: func(t *testing.T) context.Context { return cancelledWith(t, ErrRunCancelled) }},
		{name: "plain cancel", want: false, ctx: func(t *testing.T) context.Context {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			return ctx
		}},
		{name: "expired deadline", want: false, ctx: func(t *testing.T) context.Context {
			ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
			t.Cleanup(cancel)
			return ctx
		}},
		{name: "timeout child derived before a stop", want: true, ctx: func(t *testing.T) context.Context {
			parent, cancel := context.WithCancelCause(t.Context())
			child := timeoutChild(parent, t)
			cancel(ErrHostStopping)
			return child
		}},
		{name: "timeout child derived before a lost run", want: true, ctx: func(t *testing.T) context.Context {
			parent, cancel := context.WithCancelCause(t.Context())
			child := timeoutChild(parent, t)
			cancel(ErrOwnershipLost)
			return child
		}},
		{name: "timeout child derived after a stop", want: true, ctx: func(t *testing.T) context.Context {
			return timeoutChild(cancelledWith(t, ErrHostStopping), t)
		}},
		{name: "timeout child derived after a lost run", want: true, ctx: func(t *testing.T) context.Context {
			return timeoutChild(cancelledWith(t, ErrOwnershipLost), t)
		}},
		{name: "timeout child whose deadline fired before a stop", want: false, ctx: func(t *testing.T) context.Context {
			parent, cancel := context.WithCancelCause(t.Context())
			child, cancelChild := context.WithTimeout(parent, 0)
			t.Cleanup(cancelChild)
			<-child.Done()
			cancel(ErrHostStopping)
			return child
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Preempted(tc.ctx(t)); got != tc.want {
				t.Fatalf("Preempted = %v, want %v", got, tc.want)
			}
		})
	}
}
