package durable

import (
	"context"
	"slices"
	"testing"
)

type recordingInvocation struct {
	Invocation
	keys       []string
	concurrent bool
}

func (r *recordingInvocation) RunAction(ctx context.Context, key string, fn func(ctx context.Context) ([]byte, error)) ([]byte, error) {
	r.keys = append(r.keys, key)
	return fn(ctx)
}

func (r *recordingInvocation) ConcurrentActions() bool { return r.concurrent }

func TestTurnScopeWithinPrefixesEveryAction(t *testing.T) {
	const prefix = "run/r1/step/s1/attempt/1/agent/"
	cases := []struct {
		name   string
		prefix string
		want   []string
	}{
		{name: "prefixed scope", prefix: prefix, want: []string{prefix + "model-1", prefix + "clock-1", prefix + "call-1-0/policy", prefix + "call-1-0/standing-grant", prefix + "call-1-0/execution"}},
		{name: "empty prefix keeps the turn keys", prefix: "", want: []string{"model-1", "clock-1", "call-1-0/policy", "call-1-0/standing-grant", "call-1-0/execution"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inner := &recordingInvocation{}
			scope := NewTurnScopeWithin(inner, tc.prefix)
			noop := func(context.Context) ([]byte, error) { return nil, nil }
			for _, key := range []string{scope.NextModelCall(), scope.NextClock(), "call-1-0/policy", "call-1-0/standing-grant", "call-1-0/execution"} {
				if _, err := scope.Invocation().RunAction(t.Context(), key, noop); err != nil {
					t.Fatalf("RunAction(%q): %v", key, err)
				}
			}
			if !slices.Equal(inner.keys, tc.want) {
				t.Fatalf("recorded keys = %q, want %q", inner.keys, tc.want)
			}
		})
	}
}

func TestTurnScopeWithinForwardsTheConcurrentActionsDeclaration(t *testing.T) {
	for _, concurrent := range []bool{true, false} {
		scope := NewTurnScopeWithin(&recordingInvocation{concurrent: concurrent}, "p/")
		if got := ConcurrentActions(scope.Invocation()); got != concurrent {
			t.Fatalf("ConcurrentActions = %t, want %t", got, concurrent)
		}
	}
}

func TestTurnScopeWithinRefusesANilInvocation(t *testing.T) {
	if scope := NewTurnScopeWithin(nil, "p/"); scope != nil {
		t.Fatalf("scope = %v, want nil", scope)
	}
}
