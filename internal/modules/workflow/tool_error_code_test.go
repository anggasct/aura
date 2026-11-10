package workflow

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestToolStepRecordsTheCodeOfACodedRunnerError(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode string
	}{
		{name: "capability unavailable", err: codedError(ErrorCodeCapabilityUnavailable, "tool read_file needs a capability this host does not provide"), wantCode: "capability_unavailable"},
		{name: "wrapped capability unavailable", err: errors.Join(errors.New("context"), codedError(ErrorCodeCapabilityUnavailable, "x")), wantCode: "capability_unavailable"},
		{name: "other coded error", err: codedError(ErrorCodeStepTimeout, "slow"), wantCode: string(ErrorCodeStepTimeout)},
		{name: "plain error", err: errors.New("boom"), wantCode: string(ErrorCodeStepFailed)},
	}
	for _, tc := range cases {
		t.Run("started/"+tc.name, func(t *testing.T) {
			tools := &fakeToolRunner{err: tc.err}
			interpreter, _, disk := serialTestSetup(t, tools)
			loadAll(t, interpreter, toolSpec("leaf"))
			summary, err := interpreter.Start(context.Background(), "leaf", nil)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			waitForRunStatus(t, disk, summary.ID, RunFailed, 5*time.Second)
			steps, err := disk.Steps(context.Background(), summary.ID)
			if err != nil || len(steps) != 1 {
				t.Fatalf("Steps = %d, %v; want one", len(steps), err)
			}
			if steps[0].ErrorCode != tc.wantCode {
				t.Fatalf("step error code = %s, want %s", steps[0].ErrorCode, tc.wantCode)
			}
		})
		t.Run("serial/"+tc.name, func(t *testing.T) {
			tools := &fakeToolRunner{err: tc.err}
			interpreter, fake, disk := serialTestSetup(t, tools)
			runID := startSerialRun(t, interpreter, disk, toolSpec("leaf"), &RunInput{})
			inv, _ := holdInvocation(t, fake, "holder-tool-error")
			terminal, err := interpreter.DriveSerial(context.Background(), inv, runID)
			if err != nil || terminal != RunFailed {
				t.Fatalf("DriveSerial = %s, %v; want a failed run", terminal, err)
			}
			steps, err := disk.Steps(context.Background(), runID)
			if err != nil || len(steps) != 1 {
				t.Fatalf("Steps = %d, %v; want one", len(steps), err)
			}
			if steps[0].Status != StepFailed || steps[0].ErrorCode != tc.wantCode {
				t.Fatalf("step = %s/%s, want failed/%s", steps[0].Status, steps[0].ErrorCode, tc.wantCode)
			}
		})
	}
}
