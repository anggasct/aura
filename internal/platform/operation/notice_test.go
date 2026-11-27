package operation_test

import (
	"context"
	"testing"

	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

func TestOwnerNoticeRunsWithoutACardThroughItsRateLimit(t *testing.T) {
	rig := operationtest.NewRig(t, nil)

	outcome := rig.Execute(t.Context(), operationtest.Request(t, operationtest.ToolOf(operationtest.Notice), "hello"))

	if outcome.Class != toolbroker.ResultOK {
		t.Fatalf("outcome = %+v, want ok", outcome)
	}
	if prompts := rig.Prompts(); len(prompts) != 0 {
		t.Errorf("approval prompts = %d, want none for an owner notice", len(prompts))
	}
	if limited := rig.Stubs.RateLimited(); limited != 1 {
		t.Errorf("rate limit checks = %d, want 1", limited)
	}
	if handled := rig.Stubs.Handled(operationtest.Notice); handled != 1 {
		t.Errorf("handler runs = %d, want 1", handled)
	}
}

func TestOwnerNoticeOverItsRateLimitNeverRuns(t *testing.T) {
	rig := operationtest.NewRig(t, &operationtest.Options{
		Bindings: func(stubs *operationtest.Stubs) []operation.Binding {
			bindings := stubs.Bindings()
			for i := range bindings {
				if bindings[i].ID == operationtest.Notice {
					bindings[i].RateLimit = func(context.Context, *operation.Call) error {
						return operation.NewError(operation.ErrorCodeRateLimited, "too many notices")
					}
				}
			}
			return bindings
		},
	})

	t.Run("directly", func(t *testing.T) {
		_, err := rig.Registry.Invoke(t.Context(), operationtest.Notice, call(operation.SurfaceAgent, "key-1", "digest-1", "hello"))
		if !fault.HasCode(err, operation.ErrorCodeRateLimited) {
			t.Fatalf("Invoke = %v, want operation_rate_limited", err)
		}
	})
	t.Run("through the broker", func(t *testing.T) {
		outcome := rig.Execute(t.Context(), operationtest.Request(t, operationtest.ToolOf(operationtest.Notice), "hello"))
		if outcome.Class != toolbroker.ResultExecutionFailed {
			t.Fatalf("outcome = %+v, want execution_failed", outcome)
		}
		failed := intentsIn(t, rig, effect.StateFailed)
		if len(failed) != 1 || failed[0].SafeErrorCode != string(operation.ErrorCodeRateLimited) {
			t.Fatalf("failed intents = %+v, want one failed operation_rate_limited", failed)
		}
	})
	if handled := rig.Stubs.Handled(operationtest.Notice); handled != 0 {
		t.Errorf("handler runs = %d, want none over the limit", handled)
	}
}
