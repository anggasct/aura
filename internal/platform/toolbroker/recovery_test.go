package toolbroker

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/fault"
)

func noRecovery(context.Context, *ToolRequest, *effect.Intent, *RecoveryRun) error { return nil }

func TestBrokerRegistersARecoveryOnlyForAnEffectfulOperation(t *testing.T) {
	registration := func(stub operationStub) ToolRegistration {
		definition := stub.definition(t)
		built := ToolRegistration{Definition: definition, Adapter: unusedAdapter, Rule: DefaultRule(&definition), Recovery: noRecovery}
		if needsPreview(&definition) {
			built.Preview = defaultPreview
		}
		return built
	}
	cases := []struct {
		name     string
		build    ToolRegistration
		accepted bool
	}{
		{name: "a change operation", build: registration(changeStub), accepted: true},
		{name: "a destructive operation", build: registration(destructiveStub), accepted: true},
		{name: "a read operation", build: registration(readStub)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			broker := newProbeBroker(t)
			err := broker.RegisterProvider(stubOperations{tc.build})
			if tc.accepted {
				if err != nil {
					t.Fatalf("RegisterProvider = %v, want accepted", err)
				}
				if broker.recoveryFor(&tc.build.Definition) == nil {
					t.Error("the recovery was not stored")
				}
				return
			}
			if !fault.HasCode(err, ResultInvalidArgument) {
				t.Fatalf("RegisterProvider = %v, want %s", err, ResultInvalidArgument)
			}
			if broker.recoveryFor(&tc.build.Definition) != nil {
				t.Error("a rejected registration kept its recovery")
			}
		})
	}
	t.Run("a legacy tool", func(t *testing.T) {
		broker := newProbeBroker(t)
		legacy := stubOperations{{Definition: readStub.definition(t), Adapter: unusedAdapter, Recovery: noRecovery}}
		legacy[0].Definition.Operation, legacy[0].Definition.Risk = "", ""
		legacy[0].Rule = DefaultRule(&legacy[0].Definition)
		if err := broker.RegisterProvider(legacy); err == nil {
			t.Fatal("RegisterProvider accepted a recovery for a legacy tool")
		}
	})
}

func TestBrokerDropsTheRecoveryWithItsTool(t *testing.T) {
	broker := newProbeBroker(t)
	definition := changeStub.definition(t)
	registration := ToolRegistration{Definition: definition, Adapter: unusedAdapter, Rule: DefaultRule(&definition), Recovery: noRecovery}
	if err := broker.RegisterProvider(stubOperations{registration}); err != nil {
		t.Fatalf("RegisterProvider: %v", err)
	}

	broker.UnregisterTool(definition.Name, definition.Version)

	if broker.recoveryFor(&definition) != nil {
		t.Error("the recovery outlived its tool")
	}
}

func startedOperationIntent(t *testing.T, rig *operationRig) *effect.Intent {
	t.Helper()
	journal := rig.executor.Journal()
	prepared, err := journal.Prepare(t.Context(), &effect.PrepareRequest{
		SessionID: "session-1", TurnID: "turn-1", ToolCallID: "call-1", IdempotencyKey: "idempotency-1",
		Provider: operationProvider, Operation: changeStub.name, Classification: effect.ClassificationEffectful,
		Request: json.RawMessage(`{"target":"a"}`), AllocateSequence: true,
		EventInvocation: "invocation-1", EventBranch: "main", EventAuthor: "test",
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	started, err := journal.Start(t.Context(), prepared.ID)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return started
}

func TestRecoveryRereadsAnIntentThatMovedUnderIt(t *testing.T) {
	cases := []struct {
		name      string
		move      func(t *testing.T, journal *effect.Journal, held *effect.Intent)
		wantState effect.State
		wantRuns  int
	}{
		{name: "the holder settled it", wantState: effect.StateSucceeded, wantRuns: 1, move: func(t *testing.T, journal *effect.Journal, held *effect.Intent) {
			if _, err := journal.Succeed(t.Context(), held.ID, held.LeaseToken, nil); err != nil {
				t.Fatalf("Succeed: %v", err)
			}
		}},
		{name: "the holder left it unknown", wantState: effect.StateUnknown, wantRuns: 2, move: func(t *testing.T, journal *effect.Journal, held *effect.Intent) {
			if _, err := journal.MarkUnknown(t.Context(), held.ID, held.LeaseToken); err != nil {
				t.Fatalf("MarkUnknown: %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newOperationRig(t, openOperationDatabase(t), &operationOptions{}, changeStub)
			journal := rig.executor.Journal()
			held := startedOperationIntent(t, rig)
			runs := 0
			recovery := func(_ context.Context, _ *ToolRequest, seen *effect.Intent, _ *RecoveryRun) error {
				runs++
				if runs == 1 {
					tc.move(t, journal, held)
					return fault.New(effect.ErrorCodeTransitionInvalid, "the intent moved")
				}
				return nil
			}
			call := &journaledTool{canonical: operationRequest(changeStub.name, "a"), recovery: recovery}
			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()

			settled, err := rig.broker.recoverIntent(ctx, call, &effectInvocation{}, held)

			if err != nil || settled.State != tc.wantState || runs != tc.wantRuns {
				t.Fatalf("recoverIntent = %+v, %v after %d recovery runs; want %s after %d without waiting out a poll", settled, err, runs, tc.wantState, tc.wantRuns)
			}
		})
	}
}
