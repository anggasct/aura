package toolbroker

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
)

func TestBrokerEndedRequestNeverReachesTheAdapter(t *testing.T) {
	cases := []struct {
		name          string
		atEntry       bool
		prepare       func(context.CancelFunc, *ToolRequest)
		decide        func(context.CancelFunc)
		wantDecisions int
	}{
		{
			name:    "cancelled at entry",
			atEntry: true,
			prepare: func(cancel context.CancelFunc, _ *ToolRequest) { cancel() },
		},
		{
			name:    "expired at entry",
			atEntry: true,
			prepare: func(_ context.CancelFunc, request *ToolRequest) {
				request.Deadline = time.Now().Add(-time.Second)
			},
		},
		{
			name:          "cancelled after approval",
			decide:        func(cancel context.CancelFunc) { cancel() },
			wantDecisions: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			executor := newBrokerEffectExecutor(t)
			adapterCalls := 0
			decisions := 0
			var observed []Observation
			broker, err := New(&Options{
				HostCapabilities: capabilitytest.Host(t),
				Effects:          executor,
				Adapters: map[string]Adapter{
					"exec@v1": func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
						adapterCalls++
						return ToolResult{Class: ResultOK, Output: json.RawMessage(`{}`)}, nil
					},
				},
				ApprovalDecider: func(context.Context, *ApprovalPrompt) (bool, error) {
					decisions++
					if tc.decide != nil {
						tc.decide(cancel)
					}
					return true, nil
				},
				Observer: func(_ context.Context, observation Observation) {
					observed = append(observed, observation)
				},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			probe := &gateProbe{broker: broker, executor: executor}
			request := brokerRequest("exec", `{"command":["true"]}`, "exec-linux")
			request.EventSequence = 1
			if tc.prepare != nil {
				tc.prepare(cancel, request)
			}

			_, err = broker.Execute(ctx, request)

			if code, ok := CodeOf(err); !ok || code != ResultDeadlineExceeded {
				t.Fatalf("Execute error = %v, want deadline_exceeded", err)
			}
			if adapterCalls != 0 || probe.intents(t) != 0 {
				t.Fatalf("adapter calls = %d, intents = %d; want none", adapterCalls, probe.intents(t))
			}
			if decisions != tc.wantDecisions {
				t.Fatalf("approval decisions = %d, want %d", decisions, tc.wantDecisions)
			}
			if len(observed) != 1 {
				t.Fatalf("observations = %d, want 1", len(observed))
			}
			if tc.atEntry && observed[0].PolicyOutcome != PolicyOutcomeNotEvaluated {
				t.Fatalf("policy outcome = %q, want %q", observed[0].PolicyOutcome, PolicyOutcomeNotEvaluated)
			}
		})
	}
}
