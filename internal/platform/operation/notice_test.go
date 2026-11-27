package operation_test

import (
	"context"
	"testing"

	"github.com/anggasct/aura/internal/platform/effect"
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

func TestRateLimitAppliesToTheAgentSurfaceOnly(t *testing.T) {
	rig := operationtest.NewRig(t, nil)

	if _, err := rig.Registry.Invoke(t.Context(), operationtest.Notice, call(operation.SurfaceChat, "key-1", "digest-1", "hello")); err != nil {
		t.Fatalf("chat Invoke: %v", err)
	}
	if _, err := rig.Registry.Invoke(t.Context(), operationtest.Notice, call(operation.SurfaceCLI, "", "", "hello")); err != nil {
		t.Fatalf("CLI Invoke: %v", err)
	}
	if limited := rig.Stubs.RateLimited(); limited != 0 {
		t.Fatalf("rate limit checks off the agent surface = %d, want none", limited)
	}

	request := operationtest.Request(t, operationtest.ToolOf(operationtest.Notice), "hello")
	if outcome := rig.Execute(t.Context(), request); outcome.Class != toolbroker.ResultOK {
		t.Fatalf("agent outcome = %+v, want ok", outcome)
	}
	if keys := rig.Stubs.RateLimitedKeys(); len(keys) != 1 || keys[0] != request.IdempotencyKey {
		t.Errorf("rate limit keys = %v, want the call's idempotency key once", keys)
	}
}

func TestRateLimitIsNotConsultedWhenACallIsResumed(t *testing.T) {
	rig := operationtest.NewRig(t, nil)
	request := killCall(t, rig, operationtest.Notice, killBeforeCommit)
	expireLease(rig)

	outcome := rig.Execute(t.Context(), request)

	if outcome.Class != toolbroker.ResultOK {
		t.Fatalf("resumed outcome = %+v, want ok", outcome)
	}
	if keys := rig.Stubs.RateLimitedKeys(); len(keys) != 1 {
		t.Errorf("rate limit checks = %v, want only the original run's", keys)
	}
	if handled := rig.Stubs.Handled(operationtest.Notice); handled != 2 {
		t.Errorf("handler runs = %d, want the original and the resumed run", handled)
	}
}

func TestResumedRunIsNotRefusedByAnExhaustedLimit(t *testing.T) {
	exhausted := false
	rig := operationtest.NewRig(t, &operationtest.Options{
		Bindings: func(stubs *operationtest.Stubs) []operation.Binding {
			bindings := stubs.Bindings()
			for i := range bindings {
				if bindings[i].ID == operationtest.Notice {
					inner := bindings[i].RateLimit
					bindings[i].RateLimit = func(ctx context.Context, c *operation.Call) error {
						if exhausted {
							return operation.NewError(operation.ErrorCodeRateLimited, "too many notices")
						}
						return inner(ctx, c)
					}
				}
			}
			return bindings
		},
	})
	request := killCall(t, rig, operationtest.Notice, killBeforeCommit)
	expireLease(rig)
	exhausted = true

	outcome := rig.Execute(t.Context(), request)

	if outcome.Class != toolbroker.ResultOK {
		t.Fatalf("resumed outcome = %+v, want ok although the limit is exhausted", outcome)
	}
	if failed := intentsIn(t, rig, effect.StateFailed); len(failed) != 0 {
		t.Errorf("failed intents = %d, want none", len(failed))
	}
}
