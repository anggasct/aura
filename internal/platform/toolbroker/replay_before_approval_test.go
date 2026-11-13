package toolbroker

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/effect"
)

func seedIntent(t *testing.T, h *replayHarness, request *ToolRequest, settled bool) {
	t.Helper()
	payload, err := effectPayload(request)
	if err != nil {
		t.Fatalf("effectPayload: %v", err)
	}
	journal := h.executor.Journal()
	intent, err := journal.Prepare(t.Context(), &effect.PrepareRequest{
		SessionID: request.SessionID, TurnID: request.TurnID, ToolCallID: request.RequestID,
		IdempotencyKey: request.IdempotencyKey, Provider: effectProvider, Operation: request.ToolName,
		Classification: effect.ClassificationEffectful, Request: payload,
		AllocateSequence: true, EventInvocation: request.RequestID, EventBranch: "main", EventAuthor: "test",
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !settled {
		return
	}
	if _, err := journal.Start(t.Context(), intent.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := journal.Succeed(t.Context(), intent.ID, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Succeed: %v", err)
	}
}

func TestReplayOfASettledEffectIsAnsweredBeforeApproval(t *testing.T) {
	asked := 0
	h := newReplayHarness(t, &replayConfig{
		output: json.RawMessage(`{"ok":true}`), inlineLimit: 64 << 10,
		decider: func(context.Context, *ApprovalPrompt) (bool, error) {
			asked++
			return true, nil
		},
	})
	execute := func(request *ToolRequest) (ToolResult, error) {
		return h.broker.Execute(t.Context(), request)
	}

	first, err := execute(replayRequest(replayArguments))
	if err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	if asked != 1 || h.calls != 1 {
		t.Fatalf("first run: approvals = %d, adapter calls = %d, want 1 and 1", asked, h.calls)
	}

	t.Run("the same request is answered from the journal", func(t *testing.T) {
		replay, err := execute(replayRequest(replayArguments))
		if err != nil {
			t.Fatalf("replay Execute: %v", err)
		}
		requireSameResult(t, &replay, &first)
		if asked != 1 || h.calls != 1 {
			t.Fatalf("replay: approvals = %d, adapter calls = %d, want 1 and 1", asked, h.calls)
		}
	})

	t.Run("the same key with other arguments is a conflict that asks nobody", func(t *testing.T) {
		_, err := execute(replayRequest(`{"command":["false"]}`))
		if class, _ := CodeOf(err); class != ResultInvalidArgument {
			t.Fatalf("Execute = %v, want %s", err, ResultInvalidArgument)
		}
		if asked != 1 {
			t.Fatalf("approvals = %d, want 1", asked)
		}
	})

	t.Run("a prepared intent is authorized as usual", func(t *testing.T) {
		request := replayRequest(replayArguments)
		request.IdempotencyKey = "idempotency-prepared"
		seedIntent(t, h, request, false)
		if _, err := execute(request); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if asked != 2 || h.calls != 2 {
			t.Fatalf("prepared: approvals = %d, adapter calls = %d, want 2 and 2", asked, h.calls)
		}
	})

	for _, settled := range []bool{false, true} {
		name := "an intent of another session is refused while prepared"
		if settled {
			name = "an intent of another session is refused once succeeded"
		}
		t.Run(name, func(t *testing.T) {
			foreign := replayRequest(replayArguments)
			foreign.SessionID = "session-2"
			foreign.IdempotencyKey = "idempotency-foreign"
			if settled {
				foreign.IdempotencyKey += "-settled"
			}
			seedIntent(t, h, foreign, settled)
			own := replayRequest(replayArguments)
			own.IdempotencyKey = foreign.IdempotencyKey
			before, callsBefore := asked, h.calls
			_, err := execute(own)
			if class, _ := CodeOf(err); class != ResultInvalidArgument {
				t.Fatalf("Execute = %v, want %s", err, ResultInvalidArgument)
			}
			if asked != before || h.calls != callsBefore {
				t.Fatalf("approvals = %d, adapter calls = %d, want %d and %d", asked, h.calls, before, callsBefore)
			}
		})
	}
}

func TestEffectRequestMayLetTheJournalAllocateItsSequence(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*ToolRequest)
		wantFail bool
	}{
		{name: "allocation without a sequence", mutate: func(r *ToolRequest) { r.EventSequence, r.AllocateEventSequence = 0, true }},
		{name: "a sequence without allocation", mutate: func(r *ToolRequest) { r.EventSequence = 4 }},
		{name: "neither", mutate: func(r *ToolRequest) { r.EventSequence = 0 }, wantFail: true},
		{name: "both", mutate: func(r *ToolRequest) { r.EventSequence, r.AllocateEventSequence = 4, true }, wantFail: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			request := replayRequest(replayArguments)
			tc.mutate(request)
			err := validateEffectRequest(request)
			if tc.wantFail {
				if class, _ := CodeOf(err); class != ResultInvalidArgument {
					t.Fatalf("validateEffectRequest = %v, want %s", err, ResultInvalidArgument)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateEffectRequest = %v, want nil", err)
			}
		})
	}
	t.Run("allocation under a durable turn scope reaches the journal", func(t *testing.T) {
		harness := newReplayHarness(t, &replayConfig{output: json.RawMessage(`{"ok":true}`), inlineLimit: 64 << 10})
		request := replayRequest(replayArguments)
		request.EventSequence, request.AllocateEventSequence = 0, true
		ctx := durable.WithTurnScope(t.Context(), durable.NewTurnScope(passthroughInvocation{}))
		if _, err := harness.run(ctx, request); err != nil {
			t.Fatalf("Execute under a turn scope = %v, want success", err)
		}
		var sequence uint64
		if err := harness.db.QueryRowContext(t.Context(), `SELECT sequence FROM runtime_event WHERE session_id = ? AND kind = 'tool.requested'`, request.SessionID).Scan(&sequence); err != nil {
			t.Fatalf("read the tool.requested event: %v", err)
		}
		if sequence != 1 {
			t.Fatalf("tool.requested sequence = %d, want 1", sequence)
		}
	})
}
