package operation_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

type recordedOutcomes struct {
	mu       sync.Mutex
	outcomes []operationtest.Outcome
}

func (r *recordedOutcomes) add(outcome operationtest.Outcome) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outcomes = append(r.outcomes, outcome)
	return len(r.outcomes)
}

func (r *recordedOutcomes) list() []operationtest.Outcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]operationtest.Outcome(nil), r.outcomes...)
}

func actionKeys(actions []operationtest.Action) string {
	keys := make([]string, 0, len(actions))
	for _, action := range actions {
		keys = append(keys, action.Key)
	}
	return strings.Join(keys, ",")
}

func closeOnce(ch chan struct{}) func() {
	var once sync.Once
	return func() { once.Do(func() { close(ch) }) }
}

func TestOperationCallReplaysOnDurableTurn(t *testing.T) {
	db := operationtest.OpenDatabase(t)
	hosted := operationtest.StartHosted(t, db)
	rig := operationtest.NewRig(t, &operationtest.Options{DB: db})
	request := operationtest.Request(t, operationtest.ToolOf(operationtest.Delete), "a")
	var recorded recordedOutcomes
	called := make(chan struct{})
	signal := closeOnce(called)

	key := hosted.Start(t, func(ctx context.Context, inv durable.Invocation) error {
		recorded.add(rig.Execute(operationtest.InTurn(ctx, inv), request))
		if len(recorded.list()) == 1 {
			signal()
			<-ctx.Done()
		}
		return nil
	})
	<-called
	hosted.Preempt(t)
	hosted.AwaitEnded(t, key, 2)

	outcomes := recorded.list()
	if len(outcomes) != 2 || outcomes[0].Class != toolbroker.ResultOK || outcomes[0] != outcomes[1] {
		t.Fatalf("outcomes = %+v, want the call and its replay to agree", outcomes)
	}
	if handled := rig.Stubs.Handled(operationtest.Delete); handled != 1 {
		t.Errorf("handler runs = %d, want 1 across the call and its replay", handled)
	}
	if previews := rig.Stubs.Previews(); previews != 1 {
		t.Errorf("previewer runs = %d, want the journaled preview read on the replay", previews)
	}
	if prompts := rig.Prompts(); len(prompts) != 1 {
		t.Errorf("approval prompts = %d, want 1", len(prompts))
	}
	if got, want := actionKeys(hosted.Actions(t, key)), "call-1-0/policy,call-1-0/preview,call-1-0/authorization,call-1-0/execution"; got != want {
		t.Errorf("action keys = %s, want %s", got, want)
	}
}

func TestKilledAfterCommitReplaysOnce(t *testing.T) {
	db := operationtest.OpenDatabase(t)
	hosted := operationtest.StartHosted(t, db)
	rig := operationtest.NewRig(t, &operationtest.Options{DB: db})
	request := operationtest.Request(t, operationtest.ToolOf(operationtest.Change), "a")
	committed := make(chan struct{})
	signal := closeOnce(committed)
	rig.Stubs.AfterCommit(operationtest.Change, func(ctx context.Context, _ *operation.Call) error {
		signal()
		<-ctx.Done()
		return ctx.Err()
	})
	var recorded recordedOutcomes

	key := hosted.Start(t, func(ctx context.Context, inv durable.Invocation) error {
		recorded.add(rig.Execute(operationtest.InTurn(ctx, inv), request))
		return nil
	})
	<-committed
	hosted.Preempt(t)
	rig.Clock.AwaitArmed(t, 3)
	rig.Clock.Advance(61 * time.Second)
	hosted.AwaitEnded(t, key, 2)

	outcomes := recorded.list()
	if len(outcomes) != 2 || outcomes[1].Class != toolbroker.ResultOK || outcomes[1].Output != `{"done":true,"operation":"stub.change"}` {
		t.Fatalf("outcomes = %+v, want the replay to return the committed result", outcomes)
	}
	if handled := rig.Stubs.Handled(operationtest.Change); handled != 1 {
		t.Errorf("handler runs = %d, want the replay settled from the receipt", handled)
	}
	if ledger, _ := rig.Stubs.Ledger(t.Context(), operationtest.Change); ledger != 1 {
		t.Errorf("ledger rows = %d, want one application", ledger)
	}
	if succeeded := intentsIn(t, rig, effect.StateSucceeded); len(succeeded) != 1 {
		t.Errorf("succeeded intents = %d, want 1", len(succeeded))
	}
}

func TestPreemptedInvocationDoesNotRunTwice(t *testing.T) {
	db := operationtest.OpenDatabase(t)
	hosted := operationtest.StartHosted(t, db)
	rig := operationtest.NewRig(t, &operationtest.Options{DB: db})
	request := operationtest.Request(t, operationtest.ToolOf(operationtest.Change), "a")
	entered := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	signalEntered, signalFinished := closeOnce(entered), closeOnce(finished)
	rig.Stubs.BeforeCommit(operationtest.Change, func(context.Context, *operation.Call) error {
		signalEntered()
		<-release
		return nil
	})
	var recorded recordedOutcomes

	key := hosted.Start(t, func(ctx context.Context, inv durable.Invocation) error {
		callCtx := ctx
		first := len(recorded.list()) == 0
		if first {
			callCtx = context.WithoutCancel(ctx)
		}
		recorded.add(rig.Execute(operationtest.InTurn(callCtx, inv), request))
		if first {
			signalFinished()
		}
		return nil
	})
	<-entered
	hosted.Preempt(t)
	hosted.AddHost(t)
	rig.Clock.AwaitArmed(t, 3)
	if handled := rig.Stubs.Handled(operationtest.Change); handled != 1 {
		t.Fatalf("handler runs while the replay waits = %d, want only the preempted invocation's", handled)
	}
	close(release)
	<-finished
	rig.Clock.Advance(time.Second)
	hosted.AwaitEnded(t, key, 2)

	if handled := rig.Stubs.Handled(operationtest.Change); handled != 1 {
		t.Errorf("handler runs = %d, want 1", handled)
	}
	if ledger, _ := rig.Stubs.Ledger(t.Context(), operationtest.Change); ledger != 1 {
		t.Errorf("ledger rows = %d, want one application", ledger)
	}
	outcomes := recorded.list()
	if len(outcomes) != 2 || outcomes[1].Class != toolbroker.ResultOK {
		t.Errorf("outcomes = %+v, want the replay to return the recorded result", outcomes)
	}
}
