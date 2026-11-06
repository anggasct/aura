package health

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestInterpreterCheckerReportsEachRuntimeState(t *testing.T) {
	cases := []struct {
		name            string
		state           string
		reason          string
		wantCode        string
		wantStatus      Status
		wantRemediation string
		wantDetail      string
	}{
		{"installed", InterpreterRuntimeInstalled, "", "ok", StatusUp, RemediationNone, "files match the recorded digest"},
		{"absent", InterpreterRuntimeAbsent, "", "interpreter_runtime_absent", StatusDegraded, RemediationInstallInterpreter, "aura interpreter install python"},
		{"invalid", InterpreterRuntimeInvalid, "digest_mismatch", "interpreter_runtime_invalid", StatusDegraded, RemediationInstallInterpreter, "digest_mismatch"},
		{"probe failed", InterpreterRuntimeInvalid, InterpreterReasonProbeFailed, "interpreter_runtime_probe_failed", StatusDegraded, RemediationNone, "version probe failed"},
		{"pending", InterpreterRuntimePending, "", "interpreter_runtime_pending", StatusUnknown, RemediationNone, "not been verified"},
		{"unknown state", "bogus", "", "interpreter_runtime_pending", StatusUnknown, RemediationNone, "not been verified"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checker := InterpreterChecker{Runtimes: func(context.Context) ([]InterpreterRuntime, error) {
				return []InterpreterRuntime{{Runtime: "python", Version: "3.12.16", State: tc.state, Reason: tc.reason}}, nil
			}}
			findings := checker.Check(t.Context())
			if len(findings) != 1 {
				t.Fatalf("findings = %+v, want one", findings)
			}
			got := findings[0]
			if got.Component != ComponentInterpreter || got.Scope != "python" || got.Code != tc.wantCode || got.Status != tc.wantStatus || got.Remediation != tc.wantRemediation {
				t.Errorf("finding = %+v", got)
			}
			if !strings.Contains(got.Detail, tc.wantDetail) {
				t.Errorf("detail = %q, want it to contain %q", got.Detail, tc.wantDetail)
			}
			if got.ID != ComponentInterpreter+"/"+tc.wantCode+"/python" {
				t.Errorf("ID = %q", got.ID)
			}
		})
	}
}

func TestInterpreterCheckerReportsOneFindingPerRuntime(t *testing.T) {
	checker := InterpreterChecker{Runtimes: func(context.Context) ([]InterpreterRuntime, error) {
		return []InterpreterRuntime{
			{Runtime: "python", Version: "3", State: InterpreterRuntimeInstalled},
			{Runtime: "go", Version: "1", State: InterpreterRuntimeAbsent},
		}, nil
	}}
	findings := checker.Check(t.Context())
	if len(findings) != 2 || findings[0].Scope != "python" || findings[1].Scope != "go" {
		t.Fatalf("findings = %+v", findings)
	}
	if WorstStatus(findings) != StatusDegraded {
		t.Errorf("worst status = %v, want degraded", WorstStatus(findings))
	}
}

func TestInterpreterCheckerFailsSafeWithoutState(t *testing.T) {
	cases := map[string]InterpreterChecker{
		"not wired":   {},
		"read failed": {Runtimes: func(context.Context) ([]InterpreterRuntime, error) { return nil, errors.New("disk detail") }},
	}
	for name, checker := range cases {
		t.Run(name, func(t *testing.T) {
			findings := checker.Check(t.Context())
			if len(findings) != 1 || findings[0].Code != "interpreter_unknown" || findings[0].Status != StatusUnknown {
				t.Fatalf("findings = %+v, want one unknown finding", findings)
			}
			if strings.Contains(findings[0].Detail, "disk detail") {
				t.Errorf("detail leaks the cause: %q", findings[0].Detail)
			}
		})
	}
}
