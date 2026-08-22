package harness

import (
	"context"
	"testing"
)

func TestEventLogReplaysDurableLifecycleFacts(t *testing.T) {
	log := NewEventLog()
	var calls []Phase
	runner := newTestRunner(t, phasesFor(&calls, func(context.Context, PreparedInvocation) (Execution, error) {
		return Execution{State: StateSucceeded}, nil
	}), log, nil)
	if _, err := runner.Run(context.Background(), testRequest()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	replayed := log.Replay("session-1", "turn-1")
	if len(replayed) != len(phaseOrder) {
		t.Fatalf("replayed events = %d, want %d", len(replayed), len(phaseOrder))
	}
	for i, phase := range phaseOrder {
		want := "invocation." + string(phase)
		if replayed[i].Kind != want {
			t.Fatalf("replayed[%d].Kind = %q, want %q", i, replayed[i].Kind, want)
		}
	}
	first := log.Snapshot()[0]
	first.Payload[0] = 'x'
	if log.Snapshot()[0].Payload[0] == 'x' {
		t.Fatal("event log exposed mutable payload bytes")
	}
}
