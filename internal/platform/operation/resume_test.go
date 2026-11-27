package operation_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

type killPoint int

const (
	killBeforeCommit killPoint = iota
	killAfterCommit
)

func killedHook(reached chan<- struct{}) operationtest.Hook {
	var once sync.Once
	return func(ctx context.Context, _ *operation.Call) error {
		once.Do(func() { close(reached) })
		<-ctx.Done()
		return ctx.Err()
	}
}

func killCall(t *testing.T, rig *operationtest.Rig, id string, point killPoint) *toolbroker.ToolRequest {
	t.Helper()
	request := operationtest.Request(operationtest.ToolOf(id), "a")
	reached := make(chan struct{})
	if point == killAfterCommit {
		rig.Stubs.AfterCommit(id, killedHook(reached))
	} else {
		rig.Stubs.BeforeCommit(id, killedHook(reached))
	}
	ctx, kill := context.WithCancel(t.Context())
	done := make(chan operationtest.Outcome, 1)
	go func() { done <- rig.Execute(ctx, request) }()
	<-reached
	kill()
	<-done
	rig.Stubs.BeforeCommit(id, nil)
	rig.Stubs.AfterCommit(id, nil)
	return request
}

func intentsIn(t *testing.T, rig *operationtest.Rig, state effect.State) []effect.Intent {
	t.Helper()
	intents, err := rig.Executor.Journal().ListByState(t.Context(), state, 0)
	if err != nil {
		t.Fatalf("list %s intents: %v", state, err)
	}
	return intents
}

func expireLease(rig *operationtest.Rig) {
	rig.Clock.Advance(61 * time.Second)
}

func TestResumeSettlesStartedOperationIntents(t *testing.T) {
	cases := []struct {
		name        string
		id          string
		point       killPoint
		wantClass   toolbroker.ResultClass
		wantState   effect.State
		wantHandled int
		wantLedger  int
	}{
		{name: "a transactional call killed after its commit settles from the receipt", id: operationtest.Change, point: killAfterCommit, wantClass: toolbroker.ResultOK, wantState: effect.StateSucceeded, wantHandled: 1, wantLedger: 1},
		{name: "a transactional call killed before its commit runs once more", id: operationtest.Change, point: killBeforeCommit, wantClass: toolbroker.ResultOK, wantState: effect.StateSucceeded, wantHandled: 2, wantLedger: 1},
		{name: "an external idempotent call runs again under the same intent", id: operationtest.Idempotent, point: killBeforeCommit, wantClass: toolbroker.ResultOK, wantState: effect.StateSucceeded, wantHandled: 2},
		{name: "an external call is left unknown for the owner", id: operationtest.External, point: killBeforeCommit, wantClass: toolbroker.ResultExecutionFailed, wantState: effect.StateUnknown, wantHandled: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := operationtest.NewRig(t, nil)
			request := killCall(t, rig, tc.id, tc.point)
			if started := intentsIn(t, rig, effect.StateStarted); len(started) != 1 {
				t.Fatalf("started intents after the kill = %d, want 1", len(started))
			}
			expireLease(rig)

			outcome := rig.Execute(t.Context(), request)

			if outcome.Class != tc.wantClass {
				t.Fatalf("outcome = %+v, want %s", outcome, tc.wantClass)
			}
			if tc.wantClass == toolbroker.ResultOK && outcome.Output != sprintData(tc.id) {
				t.Errorf("output = %s, want the operation's data", outcome.Output)
			}
			if got := len(intentsIn(t, rig, tc.wantState)); got != 1 {
				t.Errorf("%s intents = %d, want 1", tc.wantState, got)
			}
			if got := rig.Stubs.Handled(tc.id); got != tc.wantHandled {
				t.Errorf("handler runs = %d, want %d", got, tc.wantHandled)
			}
			if ledger, _ := rig.Stubs.Ledger(t.Context(), tc.id); ledger != tc.wantLedger {
				t.Errorf("ledger rows = %d, want %d", ledger, tc.wantLedger)
			}
		})
	}
}

func sprintData(id string) string {
	return `{"done":true,"operation":"` + id + `"}`
}

func TestUnknownOperationIntentResolvesFromReceipt(t *testing.T) {
	cases := []struct {
		name          string
		id            string
		point         killPoint
		wantClass     toolbroker.ResultClass
		wantState     effect.State
		wantErrorCode string
		wantHandled   int
		wantLedger    int
	}{
		{name: "a receipt resolves the intent succeeded without a rerun", id: operationtest.Change, point: killAfterCommit, wantClass: toolbroker.ResultOK, wantState: effect.StateSucceeded, wantHandled: 1, wantLedger: 1},
		{name: "no receipt resolves the intent failed and never reruns it", id: operationtest.Change, point: killBeforeCommit, wantClass: toolbroker.ResultExecutionFailed, wantState: effect.StateFailed, wantErrorCode: string(operation.ErrorCodeNotCommitted), wantHandled: 1},
		{name: "an external call stays unknown", id: operationtest.External, point: killBeforeCommit, wantClass: toolbroker.ResultExecutionFailed, wantState: effect.StateUnknown, wantHandled: 1},
		{name: "an external idempotent call stays unknown", id: operationtest.Idempotent, point: killBeforeCommit, wantClass: toolbroker.ResultExecutionFailed, wantState: effect.StateUnknown, wantHandled: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := operationtest.NewRig(t, nil)
			request := killCall(t, rig, tc.id, tc.point)
			expireLease(rig)
			started := intentsIn(t, rig, effect.StateStarted)
			if len(started) != 1 {
				t.Fatalf("started intents after the kill = %d, want 1", len(started))
			}
			if claimed, err := rig.Executor.Journal().Claim(t.Context(), started[0].ID); err != nil || !claimed {
				t.Fatalf("Claim = %v, %v; want the expired intent claimed unknown", claimed, err)
			}

			outcome := rig.Execute(t.Context(), request)

			if outcome.Class != tc.wantClass {
				t.Fatalf("outcome = %+v, want %s", outcome, tc.wantClass)
			}
			settled := intentsIn(t, rig, tc.wantState)
			if len(settled) != 1 || settled[0].SafeErrorCode != tc.wantErrorCode {
				t.Fatalf("%s intents = %+v, want one with safe error code %q", tc.wantState, settled, tc.wantErrorCode)
			}
			if got := rig.Stubs.Handled(tc.id); got != tc.wantHandled {
				t.Errorf("handler runs = %d, want %d", got, tc.wantHandled)
			}
			if ledger, _ := rig.Stubs.Ledger(t.Context(), tc.id); ledger != tc.wantLedger {
				t.Errorf("ledger rows = %d, want %d", ledger, tc.wantLedger)
			}
		})
	}
}

func TestResumeLeavesAnOldTransactionalCallForTheOwner(t *testing.T) {
	rig := operationtest.NewRig(t, nil)
	request := killCall(t, rig, operationtest.Change, killBeforeCommit)
	rig.Clock.Advance(operation.ReceiptRetention + time.Hour)

	for attempt := range 2 {
		outcome := rig.Execute(t.Context(), request)
		if outcome.Class != toolbroker.ResultExecutionFailed {
			t.Fatalf("attempt %d outcome = %+v, want execution_failed", attempt, outcome)
		}
		if got := len(intentsIn(t, rig, effect.StateUnknown)); got != 1 {
			t.Fatalf("attempt %d unknown intents = %d, want the intent left for the owner", attempt, got)
		}
	}
	if got := rig.Stubs.Handled(operationtest.Change); got != 1 {
		t.Errorf("handler runs = %d, want the old call never rerun", got)
	}
}

func TestDuplicateCallWhileOriginalRunsWaits(t *testing.T) {
	rig := operationtest.NewRig(t, nil)
	request := operationtest.Request(operationtest.ToolOf(operationtest.Change), "a")
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	rig.Stubs.BeforeCommit(operationtest.Change, func(ctx context.Context, _ *operation.Call) error {
		once.Do(func() { close(entered) })
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	first := make(chan operationtest.Outcome, 1)
	go func() { first <- rig.Execute(t.Context(), request) }()
	<-entered
	second := make(chan operationtest.Outcome, 1)
	go func() { second <- rig.Execute(t.Context(), request) }()
	rig.Clock.AwaitArmed(t, 3)

	rig.Clock.Advance(time.Second)
	rig.Clock.AwaitArmed(t, 1)
	if handled := rig.Stubs.Handled(operationtest.Change); handled != 1 {
		t.Fatalf("handler runs while the duplicate waits = %d, want only the original", handled)
	}
	close(release)
	original := <-first
	rig.Clock.Advance(time.Second)
	duplicate := <-second

	if original.Class != toolbroker.ResultOK || duplicate != original {
		t.Fatalf("original = %+v, duplicate = %+v, want the same ok result", original, duplicate)
	}
	if handled := rig.Stubs.Handled(operationtest.Change); handled != 1 {
		t.Errorf("handler runs = %d, want 1", handled)
	}
	if ledger, _ := rig.Stubs.Ledger(t.Context(), operationtest.Change); ledger != 1 {
		t.Errorf("ledger rows = %d, want 1", ledger)
	}
}

func TestDuplicateCallGivesUpAtItsDeadline(t *testing.T) {
	rig := operationtest.NewRig(t, nil)
	request := operationtest.Request(operationtest.ToolOf(operationtest.Change), "a")
	entered := make(chan struct{})
	var once sync.Once
	rig.Stubs.BeforeCommit(operationtest.Change, func(ctx context.Context, _ *operation.Call) error {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return ctx.Err()
	})
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	first := make(chan operationtest.Outcome, 1)
	go func() { first <- rig.Execute(ctx, request) }()
	<-entered

	waiting, cancelWaiter := context.WithCancel(t.Context())
	second := make(chan operationtest.Outcome, 1)
	go func() { second <- rig.Execute(waiting, request) }()
	rig.Clock.AwaitArmed(t, 3)
	cancelWaiter()

	outcome := <-second
	if outcome.Class != toolbroker.ResultDeadlineExceeded {
		t.Fatalf("outcome = %+v, want deadline_exceeded", outcome)
	}
	stop()
	<-first
}

func TestResumeRefusesWhatItCannotSettle(t *testing.T) {
	rig := operationtest.NewRig(t, nil)
	started := &effect.Intent{ID: "intent-1", Operation: operationtest.ToolOf(operationtest.Change), State: effect.StateStarted}
	cases := []struct {
		name   string
		intent *effect.Intent
		call   *operation.Call
		want   operation.ErrorCode
	}{
		{name: "a nil intent", call: &operation.Call{}, want: operation.ErrorCodeInvalidArgument},
		{name: "a nil call", intent: started, want: operation.ErrorCodeInvalidArgument},
		{name: "an operation that is not registered", intent: &effect.Intent{Operation: "aura_stub_ghost", State: effect.StateStarted}, call: &operation.Call{}, want: operation.ErrorCodeUnknown},
		{name: "a call without the caller's recovery hooks", intent: started, call: &operation.Call{}, want: operation.ErrorCodeInvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := rig.Registry.Resume(t.Context(), tc.intent, tc.call); !fault.HasCode(err, tc.want) {
				t.Fatalf("Resume = %v, want %s", err, tc.want)
			}
		})
	}
}
