package operation_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
)

var errBoom = errors.New("boom")

func call(surface operation.Surface, key, digest, subject string) *operation.Call {
	return &operation.Call{
		Caller:         operation.Caller{Surface: surface, Class: principal.ClassOwner, PrincipalID: operationtest.OwnerID, SessionID: operationtest.SessionID},
		Input:          json.RawMessage(`{"subject":"` + subject + `"}`),
		IdempotencyKey: key,
		RequestDigest:  digest,
	}
}

func receiptCount(t *testing.T, rig *operationtest.Rig) int {
	t.Helper()
	var count int
	if err := rig.DB.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM operation_receipt`).Scan(&count); err != nil {
		t.Fatalf("count receipts: %v", err)
	}
	return count
}

func TestOperationReceiptsGiveExactlyOnce(t *testing.T) {
	t.Run("the same key twice applies once and returns the recorded result", func(t *testing.T) {
		rig := operationtest.NewRig(t, nil)
		first, err := rig.Registry.Invoke(t.Context(), operationtest.Change, call(operation.SurfaceAgent, "key-1", "digest-1", "a"))
		if err != nil {
			t.Fatalf("first Invoke: %v", err)
		}
		second, err := rig.Registry.Invoke(t.Context(), operationtest.Change, call(operation.SurfaceAgent, "key-1", "digest-1", "a"))
		if err != nil {
			t.Fatalf("second Invoke: %v", err)
		}
		if string(first.Data) != string(second.Data) {
			t.Errorf("results = %s and %s, want the recorded result twice", first.Data, second.Data)
		}
		if ledger, _ := rig.Stubs.Ledger(t.Context(), operationtest.Change); ledger != 1 {
			t.Errorf("ledger rows = %d, want one application", ledger)
		}
		if n := receiptCount(t, rig); n != 1 {
			t.Errorf("receipts = %d, want 1", n)
		}
	})
	t.Run("the same key with another request fails closed", func(t *testing.T) {
		rig := operationtest.NewRig(t, nil)
		if _, err := rig.Registry.Invoke(t.Context(), operationtest.Change, call(operation.SurfaceAgent, "key-1", "digest-1", "a")); err != nil {
			t.Fatalf("first Invoke: %v", err)
		}
		_, err := rig.Registry.Invoke(t.Context(), operationtest.Change, call(operation.SurfaceAgent, "key-1", "digest-2", "b"))
		if !fault.HasCode(err, operation.ErrorCodeKeyReused) {
			t.Fatalf("second Invoke = %v, want operation_key_reused", err)
		}
		if ledger, _ := rig.Stubs.Ledger(t.Context(), operationtest.Change); ledger != 1 {
			t.Errorf("ledger rows = %d, want the one application", ledger)
		}
	})
	t.Run("the CLI applies in a transaction and writes no receipt", func(t *testing.T) {
		rig := operationtest.NewRig(t, nil)
		for range 2 {
			if _, err := rig.Registry.Invoke(t.Context(), operationtest.Change, call(operation.SurfaceCLI, "", "", "a")); err != nil {
				t.Fatalf("Invoke: %v", err)
			}
		}
		if ledger, _ := rig.Stubs.Ledger(t.Context(), operationtest.Change); ledger != 2 {
			t.Errorf("ledger rows = %d, want an application per CLI call", ledger)
		}
		if n := receiptCount(t, rig); n != 0 {
			t.Errorf("receipts = %d, want none for the CLI", n)
		}
	})
	t.Run("concurrent calls under one key apply once", func(t *testing.T) {
		rig := operationtest.NewRig(t, nil)
		const callers = 8
		var wg sync.WaitGroup
		results := make([]string, callers)
		errs := make([]error, callers)
		for i := range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				result, err := rig.Registry.Invoke(t.Context(), operationtest.Change, call(operation.SurfaceAgent, "key-1", "digest-1", "a"))
				results[i], errs[i] = string(result.Data), err
			}()
		}
		wg.Wait()
		for i := range callers {
			if errs[i] != nil || results[i] != results[0] {
				t.Errorf("caller %d = %q, %v; want the winner's result %q", i, results[i], errs[i], results[0])
			}
		}
		if ledger, _ := rig.Stubs.Ledger(t.Context(), operationtest.Change); ledger != 1 {
			t.Errorf("ledger rows = %d, want one application", ledger)
		}
		if n := receiptCount(t, rig); n != 1 {
			t.Errorf("receipts = %d, want 1", n)
		}
	})
}

func TestChangeHandlerMustCommit(t *testing.T) {
	t.Run("a handler that skips Commit fails and leaves no row", func(t *testing.T) {
		rig := operationtest.NewRig(t, nil)
		rig.Stubs.SkipCommit(operationtest.Change)
		_, err := rig.Registry.Invoke(t.Context(), operationtest.Change, call(operation.SurfaceAgent, "key-1", "digest-1", "a"))
		if !fault.HasCode(err, operation.ErrorCodeUncommitted) {
			t.Fatalf("Invoke = %v, want operation_uncommitted", err)
		}
		if n := receiptCount(t, rig); n != 0 {
			t.Errorf("receipts = %d, want none", n)
		}
	})
	t.Run("through the broker the intent fails and nothing is written", func(t *testing.T) {
		rig := operationtest.NewRig(t, nil)
		rig.Stubs.SkipCommit(operationtest.Change)
		request := operationtest.Request(operationtest.ToolOf(operationtest.Change), "a")
		outcome := rig.Execute(t.Context(), request)
		if outcome.Class != toolbroker.ResultExecutionFailed {
			t.Fatalf("outcome = %+v, want execution_failed", outcome)
		}
		failed, err := rig.Executor.Journal().ListByState(t.Context(), effect.StateFailed, 0)
		if err != nil || len(failed) != 1 || failed[0].SafeErrorCode != string(operation.ErrorCodeUncommitted) {
			t.Fatalf("failed intents = %+v, %v; want one failed operation_uncommitted", failed, err)
		}
		if n := receiptCount(t, rig); n != 0 {
			t.Errorf("receipts = %d, want none", n)
		}
	})
	t.Run("a handler error before Commit fails the intent definitively", func(t *testing.T) {
		rig := operationtest.NewRig(t, nil)
		rig.Stubs.FailWith(operationtest.Change, operation.Fail(effect.ErrorCodeNotFound, errBoom))
		outcome := rig.Execute(t.Context(), operationtest.Request(operationtest.ToolOf(operationtest.Change), "a"))
		if outcome.Class != toolbroker.ResultExecutionFailed {
			t.Fatalf("outcome = %+v, want execution_failed", outcome)
		}
		failed, err := rig.Executor.Journal().ListByState(t.Context(), effect.StateFailed, 0)
		if err != nil || len(failed) != 1 || failed[0].SafeErrorCode != string(operation.ErrorCodeFailed) {
			t.Fatalf("failed intents = %+v, %v; want one failed operation_failed", failed, err)
		}
	})
	t.Run("a handler error from an external operation leaves the intent unknown", func(t *testing.T) {
		rig := operationtest.NewRig(t, nil)
		rig.Stubs.FailWith(operationtest.External, errBoom)
		outcome := rig.Execute(t.Context(), operationtest.Request(operationtest.ToolOf(operationtest.External), "a"))
		if outcome.Class != toolbroker.ResultExecutionFailed {
			t.Fatalf("outcome = %+v, want execution_failed", outcome)
		}
		unknown, err := rig.Executor.Journal().ListByState(t.Context(), effect.StateUnknown, 0)
		if err != nil || len(unknown) != 1 {
			t.Fatalf("unknown intents = %d, %v; want one, since an external call may have partly happened", len(unknown), err)
		}
	})
}

func TestCommitOutsideATransactionalOperationIsRefused(t *testing.T) {
	rig := operationtest.NewRig(t, nil)
	rig.Stubs.BeforeCommit(operationtest.External, func(ctx context.Context, c *operation.Call) error {
		_, err := c.Commit(ctx, func(context.Context) (operation.Result, error) { return operation.Result{}, nil })
		return err
	})
	_, err := rig.Registry.Invoke(t.Context(), operationtest.External, call(operation.SurfaceAgent, "key-1", "digest-1", "a"))
	if !fault.HasCode(err, operation.ErrorCodeInvalid) {
		t.Fatalf("Invoke = %v, want operation_invalid", err)
	}
}

func TestInvokeRefusesWhatTheSurfaceDoesNotProject(t *testing.T) {
	rig := operationtest.NewRig(t, nil)
	cases := []struct {
		name string
		id   string
		call *operation.Call
		want operation.ErrorCode
	}{
		{name: "an unknown operation", id: "stub.ghost", call: call(operation.SurfaceCLI, "", "", "a"), want: operation.ErrorCodeUnknown},
		{name: "a host operation from chat", id: operationtest.Host, call: call(operation.SurfaceChat, "key-1", "d", "a"), want: operation.ErrorCodeNotExposed},
		{name: "a host operation from the agent", id: operationtest.Host, call: call(operation.SurfaceAgent, "key-1", "d", "a"), want: operation.ErrorCodeNotExposed},
		{name: "an owner operation from the agent", id: operationtest.OwnerOnly, call: call(operation.SurfaceAgent, "key-1", "d", "a"), want: operation.ErrorCodeNotExposed},
		{name: "a call without a surface", id: operationtest.Read, call: &operation.Call{Input: json.RawMessage(`{"subject":"a"}`)}, want: operation.ErrorCodeInvalidArgument},
		{name: "a nil call", id: operationtest.Read, call: nil, want: operation.ErrorCodeInvalidArgument},
		{name: "invalid input", id: operationtest.Read, call: &operation.Call{Caller: operation.Caller{Surface: operation.SurfaceCLI}, Input: json.RawMessage(`{}`)}, want: operation.ErrorCodeInputInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := rig.Registry.Invoke(t.Context(), tc.id, tc.call)
			if !fault.HasCode(err, tc.want) {
				t.Fatalf("Invoke = %v, want %s", err, tc.want)
			}
		})
	}
	t.Run("a non-owner class from chat", func(t *testing.T) {
		denied := call(operation.SurfaceChat, "key-1", "d", "a")
		denied.Caller.Class = principal.ClassTrusted
		if _, err := rig.Registry.Invoke(t.Context(), operationtest.Change, denied); !fault.HasCode(err, operation.ErrorCodeDenied) {
			t.Fatalf("Invoke = %v, want operation_denied", err)
		}
	})
	for _, id := range []string{operationtest.Host, operationtest.OwnerOnly} {
		if rig.Stubs.Handled(id) != 0 {
			t.Errorf("the handler of %s ran for a refused call", id)
		}
	}
}

func TestGeneratedToolsCarryTheirDefinition(t *testing.T) {
	rig := operationtest.NewRig(t, nil)
	defs := map[string]tools.Definition{}
	for _, definition := range rig.Broker.Definitions() {
		defs[definition.Name] = definition
	}
	generated := 0
	for _, descriptor := range rig.Registry.Descriptors() {
		definition, found := defs[operation.ToolName(descriptor.ID)]
		if descriptor.Exposure != operation.ExposureAgent {
			if found {
				t.Errorf("%s generated a tool and is not an agent operation", descriptor.ID)
			}
			continue
		}
		generated++
		if !found {
			t.Errorf("%s generated no tool", descriptor.ID)
			continue
		}
		flags, _ := tools.FlagsFor(tools.Risk(descriptor.Risk), descriptor.FreshApproval)
		schema, err := descriptor.SchemaFor(operation.SurfaceAgent)
		if err != nil {
			t.Fatalf("SchemaFor %s: %v", descriptor.ID, err)
		}
		switch {
		case definition.Version != tools.Version, definition.Operation != descriptor.ID, definition.Risk != tools.Risk(descriptor.Risk):
			t.Errorf("%s: version %q operation %q risk %q", descriptor.ID, definition.Version, definition.Operation, definition.Risk)
		case definition.Effectful != flags.Effectful, definition.RequiresApproval != flags.RequiresApproval,
			definition.NoStandingGrant != flags.NoStandingGrant, definition.ExactDisplay != flags.ExactDisplay:
			t.Errorf("%s: flags do not follow the risk mapping", descriptor.ID)
		case !definition.OptIn || !definition.OwnerOptIn:
			t.Errorf("%s is not opt-in and owner opt-in", descriptor.ID)
		case definition.FreshApproval != descriptor.FreshApproval, definition.OwnerNotice != descriptor.OwnerNotice,
			definition.UntrustedOutput != descriptor.UntrustedOutput, definition.ExternalSink != descriptor.Sink:
			t.Errorf("%s: fresh approval, notice, untrusted output or sink differ", descriptor.ID)
		case string(definition.Schema) != string(schema):
			t.Errorf("%s: the schema is not the generated one", descriptor.ID)
		case len(definition.Display) != 1 || definition.Display[0] != "subject":
			t.Errorf("%s: display = %v, want the parameter order", descriptor.ID, definition.Display)
		}
	}
	if generated != 8 {
		t.Errorf("generated tools = %d, want the eight agent operations", generated)
	}
}

func TestOperationToolsRunThroughTheBroker(t *testing.T) {
	rig := operationtest.NewRig(t, nil)
	outcome := rig.Execute(t.Context(), operationtest.Request(operationtest.ToolOf(operationtest.Read), "a"))
	if outcome.Class != toolbroker.ResultOK || outcome.Output != `{"done":true,"operation":"stub.read"}` {
		t.Fatalf("read outcome = %+v, want ok with the handler's data", outcome)
	}
	changed := rig.Execute(t.Context(), operationtest.Request(operationtest.ToolOf(operationtest.Change), "a"))
	if changed.Class != toolbroker.ResultOK {
		t.Fatalf("change outcome = %+v, want ok", changed)
	}
	if ledger, _ := rig.Stubs.Ledger(t.Context(), operationtest.Change); ledger != 1 {
		t.Errorf("ledger rows = %d, want 1", ledger)
	}
	if n := receiptCount(t, rig); n != 1 {
		t.Errorf("receipts = %d, want the change's one receipt", n)
	}
	invalid := operationtest.Request(operationtest.ToolOf(operationtest.Change), "a")
	invalid.Arguments = json.RawMessage(`{"subject":"a","extra":1}`)
	if rejected := rig.Execute(t.Context(), invalid); rejected.Class != toolbroker.ResultInvalidArgument {
		t.Errorf("unknown argument outcome = %+v, want invalid_argument", rejected)
	}
}

func TestOperationReplayOnANonDurableTurnReturnsTheRecordedResult(t *testing.T) {
	rig := operationtest.NewRig(t, nil)
	request := operationtest.Request(operationtest.ToolOf(operationtest.Change), "a")
	first := rig.Execute(t.Context(), request)
	again := rig.Execute(t.Context(), request)
	if first.Class != toolbroker.ResultOK || first != again {
		t.Fatalf("first = %+v, again = %+v, want the same ok result", first, again)
	}
	if rig.Stubs.Handled(operationtest.Change) != 1 {
		t.Errorf("handler runs = %d, want 1", rig.Stubs.Handled(operationtest.Change))
	}
}
