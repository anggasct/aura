package workflow

import (
	"slices"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
)

func TestNonRetryableStepCodesAreFinal(t *testing.T) {
	drivers := []struct {
		name   string
		driver Driver
	}{
		{name: "serial", driver: DriverSerial},
		{name: "concurrent", driver: DriverConcurrent},
	}
	tests := []struct {
		name  string
		code  ErrorCode
		calls int
	}{
		{name: "approval rejected", code: ErrorCodeApprovalRejected, calls: 1},
		{name: "approval missing", code: ErrorCodeApprovalMissing, calls: 1},
		{name: "approval spent", code: ErrorCodeApprovalSpent, calls: 1},
		{name: "plan unshowable", code: ErrorCodePlanUnshowable, calls: 1},
		{name: "capability exceeded", code: ErrorCodeCapabilityExceeded, calls: 1},
		{name: "persona missing", code: ErrorCodePersonaMissing, calls: 1},
		{name: "session invalid", code: ErrorCodeSessionInvalid, calls: 1},
		{name: "executor invalid", code: ErrorCodeExecutorInvalid, calls: 1},
		{name: "step timeout is retried", code: ErrorCodeStepTimeout, calls: 3},
		{name: "step failed is retried", code: ErrorCodeStepFailed, calls: 3},
	}
	for _, driver := range drivers {
		for _, tc := range tests {
			t.Run(driver.name+"/"+tc.name, func(t *testing.T) {
				tool := "read_file"
				spec := &Spec{
					ID: "final-" + driver.name + "-" + string(tc.code), Goal: "Final codes", Version: 1, Source: SourceDefined,
					Steps: []StepSpec{{
						ID: "act", Executor: ExecutorSpec{Kind: KindTool, ToolID: &tool}, Timeout: 5 * time.Second,
						Retry: RetryPolicy{Attempts: 2, Backoff: time.Millisecond},
					}},
				}
				runner := &fakeToolRunner{err: codedError(tc.code, "refused")}
				disk := newTestStore(t)
				interpreter := NewInterpreter(disk, durable.NewFake(), &Options{Tools: runner, Driver: driver.driver})
				if err := interpreter.Install(t.Context(), []*Spec{spec}, testValidationDeps()); err != nil {
					t.Fatalf("Install: %v", err)
				}
				summary, err := interpreter.Start(t.Context(), spec.ID, nil, ownerOrigin())
				if err != nil {
					t.Fatalf("Start: %v", err)
				}
				waitForRunStatus(t, disk, summary.ID, RunFailed, 5*time.Second)
				runner.mu.Lock()
				calls := slices.Clone(runner.called)
				runner.mu.Unlock()
				if len(calls) != tc.calls {
					t.Fatalf("the runner was invoked %d times, want %d", len(calls), tc.calls)
				}
			})
		}
	}
}
