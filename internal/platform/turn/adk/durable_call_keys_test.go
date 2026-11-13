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
	close(rig.gated.release)

	awaitStream(t, rig.run(context.WithoutCancel(t.Context())))
	waitFor(t, "the turn is released", func() bool { return slices.Contains(rig.actionKeys(t), "release") })

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
	wantIdempotency := []string{"adk/session-1/turn-1/call-1-0", "adk/session-1/turn-1/call-1-1"}
	got := rig.gated.seenKeys()
	slices.Sort(got)
	if !slices.Equal(got, wantIdempotency) {
		t.Errorf("idempotency keys = %q, want %q", got, wantIdempotency)
	}
}

func TestReplayedToolCallsKeepTheirOwnRecords(t *testing.T) {
	cases := []struct {
		name     string
		stalled  string
		recorded string
	}{
		{name: "call 0 ends while call 1 blocks", stalled: "b", recorded: "call-1-0/execution"},
		{name: "call 1 ends while call 0 blocks", stalled: "a", recorded: "call-1-1/execution"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model := &toolThenTextModel{queries: []string{"a", "b"}}
			rig := startDurableTurnRig(t, model)
			rig.gated.stalled = tc.stalled
			close(rig.gated.release)
			rig.run(context.WithoutCancel(t.Context()))
			waitFor(t, "one call is recorded and the other is running", func() bool {
				return rig.gated.runsOf("a") == 1 && rig.gated.runsOf("b") == 1 && slices.Contains(rig.actionKeys(t), tc.recorded)
			})

			rig.stopHost()
			rig.startLife(t)
			waitFor(t, "the second life ends the turn", func() bool { return rig.turnEnded(t) })

			for _, query := range []string{"a", "b"} {
				want := 1
				if query == tc.stalled {
					want = 2
				}
				if got := rig.gated.runsOf(query); got != want {
					t.Errorf("runs of %s = %d, want %d", query, got, want)
				}
			}
			requests := model.seen()
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
		})
	}
}
