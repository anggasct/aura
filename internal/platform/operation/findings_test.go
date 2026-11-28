package operation_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

type moduleCode string

const capabilityUnavailable moduleCode = "capability_unavailable"

type resumeProvider struct {
	mu   sync.Mutex
	runs int
}

func (p *resumeProvider) SupportsIdempotency() bool { return false }

func (p *resumeProvider) SupportsTakeover() bool { return true }

func (p *resumeProvider) Invoke(context.Context, *effect.Invocation) (effect.Outcome, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.runs++
	return effect.Outcome{Succeeded: true}, nil
}

func recoveryRun(provider *resumeProvider) *toolbroker.RecoveryRun {
	return &toolbroker.RecoveryRun{
		Provider: provider,
		Settle: func(_ context.Context, data json.RawMessage) (effect.Outcome, error) {
			stored, err := json.Marshal(toolbroker.ToolResult{Class: toolbroker.ResultOK, Output: data})
			return effect.Outcome{Succeeded: true, Result: stored}, err
		},
	}
}

func chatCall(t *testing.T, rig *operationtest.Rig, key string) *operation.Call {
	t.Helper()
	var digest string
	err := rig.DB.QueryRowContext(t.Context(), `SELECT request_digest FROM operation_receipt WHERE idempotency_key = ?`, key).Scan(&digest)
	if err != nil {
		digest = "digest"
	}
	return call(operation.SurfaceChat, key, digest, "a")
}

func TestResumeRereadsAnIntentThatMovedBeforeTheTakeover(t *testing.T) {
	const recorded = `{"done":true,"operation":"stub.change"}`
	t.Run("the holder settled it after the snapshot", func(t *testing.T) {
		rig := operationtest.NewRig(t, nil)
		request := killCall(t, rig, operationtest.Change, killBeforeCommit)
		stale := intentsIn(t, rig, effect.StateStarted)[0]
		expireLease(rig)
		if outcome := rig.Execute(t.Context(), request); outcome.Class != toolbroker.ResultOK {
			t.Fatalf("settling Execute = %+v, want ok", outcome)
		}
		provider := &resumeProvider{}

		result, err := rig.Registry.Resume(t.Context(), &stale, chatCall(t, rig, stale.IdempotencyKey), recoveryRun(provider))

		if err != nil || string(result.Data) != recorded {
			t.Fatalf("Resume = %s, %v; want the recorded result of the original", result.Data, err)
		}
		if provider.runs != 0 || rig.Stubs.Handled(operationtest.Change) != 2 {
			t.Errorf("provider runs = %d, handler runs = %d; want no further run", provider.runs, rig.Stubs.Handled(operationtest.Change))
		}
	})
	t.Run("the holder left it unknown with a receipt", func(t *testing.T) {
		rig := operationtest.NewRig(t, nil)
		killCall(t, rig, operationtest.Change, killAfterCommit)
		stale := intentsIn(t, rig, effect.StateStarted)[0]
		expireLease(rig)
		if claimed, err := rig.Executor.Journal().Claim(t.Context(), stale.ID); err != nil || !claimed {
			t.Fatalf("Claim = %v, %v", claimed, err)
		}

		result, err := rig.Registry.Resume(t.Context(), &stale, chatCall(t, rig, stale.IdempotencyKey), recoveryRun(&resumeProvider{}))

		if err != nil || string(result.Data) != recorded {
			t.Fatalf("Resume = %s, %v; want the receipt's result", result.Data, err)
		}
		if got := len(intentsIn(t, rig, effect.StateSucceeded)); got != 1 {
			t.Errorf("succeeded intents = %d, want 1", got)
		}
	})
	t.Run("the holder left it unknown without a receipt", func(t *testing.T) {
		rig := operationtest.NewRig(t, nil)
		killCall(t, rig, operationtest.Change, killBeforeCommit)
		stale := intentsIn(t, rig, effect.StateStarted)[0]
		expireLease(rig)
		if claimed, err := rig.Executor.Journal().Claim(t.Context(), stale.ID); err != nil || !claimed {
			t.Fatalf("Claim = %v, %v", claimed, err)
		}
		provider := &resumeProvider{}

		_, err := rig.Registry.Resume(t.Context(), &stale, chatCall(t, rig, stale.IdempotencyKey), recoveryRun(provider))

		if !fault.HasCode(err, operation.ErrorCodeNotCommitted) {
			t.Fatalf("Resume = %v, want operation_not_committed", err)
		}
		if provider.runs != 0 || rig.Stubs.Handled(operationtest.Change) != 1 {
			t.Errorf("provider runs = %d, handler runs = %d; want the call never rerun", provider.runs, rig.Stubs.Handled(operationtest.Change))
		}
	})
}

func TestSecondCommitReturnsTheFirstCommitsResult(t *testing.T) {
	const recorded = `{"done":true,"operation":"stub.change"}`
	observed := newObservedInvocations(t)
	rig := observed.rig
	var secondErr error
	rig.Stubs.AfterCommit(operationtest.Change, func(ctx context.Context, c *operation.Call) error {
		_, secondErr = c.Commit(ctx, func(context.Context) (operation.Result, error) { return operation.Result{}, nil })
		return nil
	})
	request := operationtest.Request(t, operationtest.ToolOf(operationtest.Change), sensitiveSubject)

	outcome := rig.Execute(t.Context(), request)

	if !fault.HasCode(secondErr, operation.ErrorCodeUncommitted) {
		t.Fatalf("second Commit = %v, want operation_uncommitted for the handler", secondErr)
	}
	if outcome.Class != toolbroker.ResultOK || outcome.Output != recorded {
		t.Fatalf("outcome = %+v, want the first commit's result as success", outcome)
	}
	if got := len(intentsIn(t, rig, effect.StateSucceeded)); got != 1 {
		t.Fatalf("succeeded intents = %d, want 1", got)
	}
	if ledger, _ := rig.Stubs.Ledger(t.Context(), operationtest.Change); ledger != 1 {
		t.Errorf("ledger rows = %d, want the first commit's one", ledger)
	}
	rig.Stubs.AfterCommit(operationtest.Change, nil)
	if replayed := rig.Execute(t.Context(), request); replayed != outcome || rig.Stubs.Handled(operationtest.Change) != 1 {
		t.Errorf("replay = %+v with %d handler runs; want the same result without a rerun", replayed, rig.Stubs.Handled(operationtest.Change))
	}
	assertBrokenContractIsVisible(t, observed)
}

func TestHandlerErrorAfterItsCommitReturnsTheCommittedResult(t *testing.T) {
	const recorded = `{"done":true,"operation":"stub.change"}`
	observed := newObservedInvocations(t)
	rig := observed.rig
	rig.Stubs.AfterCommit(operationtest.Change, func(context.Context, *operation.Call) error { return errBoom })
	request := operationtest.Request(t, operationtest.ToolOf(operationtest.Change), sensitiveSubject)

	outcome := rig.Execute(t.Context(), request)

	if outcome.Class != toolbroker.ResultOK || outcome.Output != recorded {
		t.Fatalf("outcome = %+v, want the committed result: the change was applied", outcome)
	}
	if got := len(intentsIn(t, rig, effect.StateSucceeded)); got != 1 {
		t.Fatalf("succeeded intents = %d, want 1", got)
	}
	if ledger, _ := rig.Stubs.Ledger(t.Context(), operationtest.Change); ledger != 1 {
		t.Errorf("ledger rows = %d, want 1", ledger)
	}
	assertBrokenContractIsVisible(t, observed)
}

func assertBrokenContractIsVisible(t *testing.T, observed *observedInvocations) {
	t.Helper()
	counts := observed.counts(t)
	if got := counts[invocationAttributes(operationtest.Change, "agent", "change", "failed")]; got != 1 {
		t.Errorf("failed invocations = %d, want the broken handler counted once (%v)", got, counts)
	}
	var records []string
	for _, line := range strings.Split(strings.TrimSpace(observed.logs.String()), "\n") {
		if strings.Contains(line, `"component":"operation"`) {
			records = append(records, line)
		}
	}
	if len(records) != 1 {
		t.Fatalf("operation records = %d, want one: %s", len(records), observed.logs.String())
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(records[0]), &record); err != nil {
		t.Fatalf("decode the record: %v", err)
	}
	if record["level"] != "ERROR" || record["msg"] != "operation invoked" || record["status"] != "failed" {
		t.Errorf("record = %v, want the operation invoked record at error level with status failed", record)
	}
	if strings.Contains(records[0], sensitiveSubject) || strings.Contains(records[0], "boom") {
		t.Errorf("the record carries an input or an error text: %s", records[0])
	}
}

func TestHandlerCanDeclareADefinitiveRefusal(t *testing.T) {
	cases := []struct {
		name string
		id   string
	}{
		{name: "an external operation", id: operationtest.External},
		{name: "an external idempotent operation", id: operationtest.Idempotent},
		{name: "a transactional operation", id: operationtest.Change},
	}
	for _, tc := range cases {
		t.Run(tc.name+" settles failed with the module's code", func(t *testing.T) {
			rig := operationtest.NewRig(t, nil)
			rig.Stubs.FailWith(tc.id, operation.NotPerformed(capabilityUnavailable, errBoom))

			outcome := rig.Execute(t.Context(), operationtest.Request(t, operationtest.ToolOf(tc.id), "a"))

			if outcome.Class != toolbroker.ResultExecutionFailed {
				t.Fatalf("outcome = %+v, want execution_failed", outcome)
			}
			failed := intentsIn(t, rig, effect.StateFailed)
			if len(failed) != 1 || failed[0].SafeErrorCode != string(capabilityUnavailable) {
				t.Fatalf("failed intents = %+v, want one failed %s", failed, capabilityUnavailable)
			}
			if unknown := intentsIn(t, rig, effect.StateUnknown); len(unknown) != 0 {
				t.Errorf("unknown intents = %d, want none", len(unknown))
			}
		})
	}
	t.Run("a typed operation code settles the intent with that code", func(t *testing.T) {
		rig := operationtest.NewRig(t, nil)
		rig.Stubs.FailWith(operationtest.External, operation.NotPerformed(operation.ErrorCodePreviewStale, nil))

		rig.Execute(t.Context(), operationtest.Request(t, operationtest.ToolOf(operationtest.External), "a"))

		failed := intentsIn(t, rig, effect.StateFailed)
		if len(failed) != 1 || failed[0].SafeErrorCode != string(operation.ErrorCodePreviewStale) {
			t.Fatalf("failed intents = %+v, want one failed operation_preview_stale", failed)
		}
	})
	t.Run("any other error from an external operation stays unknown", func(t *testing.T) {
		rig := operationtest.NewRig(t, nil)
		rig.Stubs.FailWith(operationtest.External, operation.Fail(capabilityUnavailable, errBoom))

		rig.Execute(t.Context(), operationtest.Request(t, operationtest.ToolOf(operationtest.External), "a"))

		if unknown := intentsIn(t, rig, effect.StateUnknown); len(unknown) != 1 {
			t.Fatalf("unknown intents = %d, want the one ambiguous call", len(unknown))
		}
		if failed := intentsIn(t, rig, effect.StateFailed); len(failed) != 0 {
			t.Errorf("failed intents = %d, want none", len(failed))
		}
	})
	t.Run("a refusal after the commit is not a refusal", func(t *testing.T) {
		rig := operationtest.NewRig(t, nil)
		rig.Stubs.AfterCommit(operationtest.Change, func(context.Context, *operation.Call) error {
			return operation.NotPerformed(capabilityUnavailable, errBoom)
		})

		rig.Execute(t.Context(), operationtest.Request(t, operationtest.ToolOf(operationtest.Change), "a"))

		if failed := intentsIn(t, rig, effect.StateFailed); len(failed) != 0 {
			t.Fatalf("failed intents = %+v, want none after a commit", failed)
		}
	})
}

func TestHandlerCannotReturnABrokerRefusalClass(t *testing.T) {
	rig := operationtest.NewRig(t, nil)
	rig.Stubs.FailWith(operationtest.Read, toolbroker.Errorf(toolbroker.ResultOwnerTurnRequired, "raw handler text %s", "secret-detail"))

	outcome := rig.Execute(t.Context(), operationtest.Request(t, operationtest.ToolOf(operationtest.Read), "a"))

	if outcome.Class != toolbroker.ResultExecutionFailed {
		t.Fatalf("outcome = %+v, want execution_failed and not the handler's class", outcome)
	}
	if outcome.Err != nil && containsText(outcome.Err.Error(), "secret-detail") {
		t.Errorf("the handler's raw text reached the broker error: %v", outcome.Err)
	}
}

func containsText(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func TestExportedEntriesRefuseTheAgentSurface(t *testing.T) {
	rig := operationtest.NewRig(t, nil)
	agent := call(operation.SurfaceAgent, "key-1", "digest-1", "a")
	agent.Caller.Class = principal.ClassOwner

	if _, err := rig.Registry.Invoke(t.Context(), operationtest.Change, agent); !fault.HasCode(err, operation.ErrorCodeNotExposed) {
		t.Fatalf("Invoke = %v, want operation_not_exposed", err)
	}
	if _, err := rig.Registry.Preview(t.Context(), operationtest.Delete, agent); !fault.HasCode(err, operation.ErrorCodeNotExposed) {
		t.Fatalf("Preview = %v, want operation_not_exposed", err)
	}
	if rig.Stubs.Handled(operationtest.Change) != 0 || rig.Stubs.Previews() != 0 {
		t.Error("an agent-surface call ran outside the broker")
	}
}

func TestCancellationAtALapseLeavesTheCallForResume(t *testing.T) {
	rig := operationtest.NewRig(t, nil)
	if _, err := rig.DB.ExecContext(t.Context(), `
CREATE TRIGGER block_lease_renewal
BEFORE UPDATE OF lease_expires_at ON effect_intent
WHEN OLD.state = 'started' AND NEW.state = 'started' AND NEW.lease_token IS OLD.lease_token
BEGIN
    SELECT RAISE(ABORT, 'renewal blocked');
END`); err != nil {
		t.Fatalf("block the lease renewals: %v", err)
	}
	request := operationtest.Request(t, operationtest.ToolOf(operationtest.Change), "a")
	reached := make(chan struct{})
	var once sync.Once
	rig.Stubs.BeforeCommit(operationtest.Change, func(ctx context.Context, _ *operation.Call) error {
		once.Do(func() { close(reached) })
		<-ctx.Done()
		return ctx.Err()
	})
	done := make(chan operationtest.Outcome, 1)
	go func() { done <- rig.Execute(t.Context(), request) }()
	<-reached
	rig.Clock.AwaitArmed(t, 2)

	rig.Clock.Advance(61 * time.Second)

	lapsed := <-done
	if lapsed.Class != toolbroker.ResultExecutionFailed {
		t.Fatalf("lapsed call = %+v, want execution_failed", lapsed)
	}
	if started := intentsIn(t, rig, effect.StateStarted); len(started) != 1 {
		t.Fatalf("started intents = %d, want the call left for Resume", len(started))
	}
	if failed := intentsIn(t, rig, effect.StateFailed); len(failed) != 0 {
		t.Fatalf("failed intents = %+v, want none: a cancellation settles nothing", failed)
	}
	rig.Stubs.BeforeCommit(operationtest.Change, nil)

	replayed := rig.Execute(t.Context(), request)

	if replayed.Class != toolbroker.ResultOK {
		t.Fatalf("replay = %+v, want the call run once under a new holder", replayed)
	}
	if ledger, _ := rig.Stubs.Ledger(t.Context(), operationtest.Change); ledger != 1 {
		t.Errorf("ledger rows = %d, want one application", ledger)
	}
}

func TestACancelledHandlerIsNeverReportedAsNotPerformed(t *testing.T) {
	rig := operationtest.NewRig(t, nil)
	rig.Stubs.FailWith(operationtest.Change, context.Canceled)

	outcome := rig.Execute(t.Context(), operationtest.Request(t, operationtest.ToolOf(operationtest.Change), "a"))

	if outcome.Class == toolbroker.ResultOK {
		t.Fatalf("outcome = %+v, want the cancellation reported", outcome)
	}
	if failed := intentsIn(t, rig, effect.StateFailed); len(failed) != 0 {
		t.Errorf("failed intents = %+v, want none for a cancellation", failed)
	}
}

func TestWriteGuardCatchesAHandlerThatWritesBeforeCommit(t *testing.T) {
	cases := []struct {
		name  string
		write bool
		want  bool
	}{
		{name: "a handler that writes only inside Commit", write: false, want: false},
		{name: "a handler that writes before Commit", write: true, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := operationtest.NewRig(t, nil)
			if tc.write {
				rig.Stubs.BeforeCommit(operationtest.Change, func(ctx context.Context, _ *operation.Call) error {
					_, err := rig.DB.ExecContext(ctx, `INSERT INTO operation_stub_ledger (operation, subject) VALUES ('early', 'x')`)
					return err
				})
			}
			problem := operationtest.WritesOutsideCommit(t, &operationtest.WriteProbe{Registry: rig.Registry, DB: rig.DB, ID: operationtest.Change, Input: json.RawMessage(`{"subject":"a"}`)})
			if (problem != "") != tc.want {
				t.Fatalf("problem = %q, want a problem: %v", problem, tc.want)
			}
		})
	}
}
