package runtimeadk

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/turn"
)

func (r *durableTurnRig) actionKeys(t *testing.T) []string {
	t.Helper()
	rows, err := r.db.QueryContext(t.Context(), `SELECT action_key FROM durable_action WHERE run_key = ? ORDER BY action_key`, "turn-1")
	if err != nil {
		t.Fatalf("read the action keys of turn-1: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatalf("scan an action key of turn-1: %v", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the action keys of turn-1: %v", err)
	}
	return keys
}

func (r *durableTurnRig) turnEnded(t *testing.T) bool {
	t.Helper()
	events := r.turnEvents(t)
	for i := range events {
		switch events[i].Kind {
		case turn.EventKindTurnCompleted, turn.EventKindTurnFailed, turn.EventKindTurnCancelled:
			return true
		}
	}
	return false
}

func TestDurableToolCallKeysFollowTheCall(t *testing.T) {
	rig := startDurableTurnRig(t, &toolThenTextModel{queries: []string{"a", "b"}})
	close(rig.tools.release)

	awaitStream(t, rig.run(context.WithoutCancel(t.Context())))

	var keys []string
	clocks := 0
	for _, key := range rig.actionKeys(t) {
		if strings.HasPrefix(key, "clock-") {
			clocks++
			continue
		}
		keys = append(keys, key)
	}
	want := []string{
		"call-1-0/authorization", "call-1-0/execution", "call-1-0/policy",
		"call-1-1/authorization", "call-1-1/execution", "call-1-1/policy",
		"delivery", "model-1", "model-2", "release", "turn-start",
	}
	if !slices.Equal(keys, want) {
		t.Errorf("action keys = %q, want %q", keys, want)
	}
	if clocks == 0 {
		t.Error("clock keys = 0, want the stamp of the terminal event")
	}
	wantIdempotency := []string{"adk/session-1/turn-1/adk-turn-1-model-1-0", "adk/session-1/turn-1/adk-turn-1-model-1-1"}
	if got := rig.tools.seenKeys(); !slices.Equal(got, wantIdempotency) {
		t.Errorf("idempotency keys = %q, want %q", got, wantIdempotency)
	}
}

func TestReplayedToolCallsKeepTheirOwnRecords(t *testing.T) {
	rig := startDurableTurnRig(t, &toolThenTextModel{queries: []string{"a", "b"}})
	rig.tools.stalled = "b"
	close(rig.tools.release)
	rig.run(context.WithoutCancel(t.Context()))
	waitFor(t, "call 1 is running with call 0 recorded", func() bool { return rig.tools.runsOf("a") == 1 && rig.tools.runsOf("b") == 1 })

	rig.stopHost()
	rig.startLife(t)
	waitFor(t, "the second life ends the turn", func() bool { return rig.turnEnded(t) })

	if got := rig.tools.runsOf("a"); got != 1 {
		t.Errorf("call 0 runs = %d, want 1", got)
	}
	if got := rig.tools.runsOf("b"); got != 2 {
		t.Errorf("call 1 runs = %d, want 2", got)
	}
	requests := rig.model.seen()
	if len(requests) != 2 {
		t.Fatalf("model calls = %d, want 2", len(requests))
	}
	var outputs []string
	for _, part := range requests[1] {
		if isResponse(part) {
			outputs = append(outputs, part.output)
		}
	}
	if len(outputs) != 2 || !strings.Contains(outputs[0], `"query":"a"`) || !strings.Contains(outputs[1], `"query":"b"`) {
		t.Errorf("function responses in the next request = %q, want call 0's output then call 1's", outputs)
	}
}
