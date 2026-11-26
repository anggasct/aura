package codeexec

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/modules/codeexec/interpreter"
	"github.com/anggasct/aura/internal/platform/health"
)

func TestInterpreterCheckerReportsEachRuntimeState(t *testing.T) {
	cases := []struct {
		name            string
		state           string
		reason          string
		wantCode        string
		wantStatus      health.Status
		wantRemediation string
		wantDetail      string
	}{
		{"installed", RuntimeStateInstalled, "", "ok", health.StatusUp, health.RemediationNone, "files match the recorded digest"},
		{"absent", RuntimeStateAbsent, "", "interpreter_runtime_absent", health.StatusDegraded, RemediationInstallInterpreter, "aura interpreter install python"},
		{"invalid", RuntimeStateInvalid, "digest_mismatch", "interpreter_runtime_invalid", health.StatusDegraded, RemediationInstallInterpreter, "digest_mismatch"},
		{"probe failed", RuntimeStateInvalid, ReasonProbeFailed, "interpreter_runtime_probe_failed", health.StatusDegraded, health.RemediationNone, "version probe failed"},
		{"pending", RuntimeStatePending, "", "interpreter_runtime_pending", health.StatusUnknown, health.RemediationNone, "not been verified"},
		{"unknown state", "bogus", "", "interpreter_runtime_pending", health.StatusUnknown, health.RemediationNone, "not been verified"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checker := HealthChecker{Runtimes: func(context.Context) ([]RuntimeHealth, error) {
				return []RuntimeHealth{{Runtime: "python", Version: "3.12.16", State: tc.state, Reason: tc.reason}}, nil
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
	checker := HealthChecker{Runtimes: func(context.Context) ([]RuntimeHealth, error) {
		return []RuntimeHealth{
			{Runtime: "python", Version: "3", State: RuntimeStateInstalled},
			{Runtime: "go", Version: "1", State: RuntimeStateAbsent},
		}, nil
	}}
	findings := checker.Check(t.Context())
	if len(findings) != 2 || findings[0].Scope != "python" || findings[1].Scope != "go" {
		t.Fatalf("findings = %+v", findings)
	}
	if health.WorstStatus(findings) != health.StatusDegraded {
		t.Errorf("worst status = %v, want degraded", health.WorstStatus(findings))
	}
}

func TestInterpreterCheckerFailsSafeWithoutState(t *testing.T) {
	cases := map[string]HealthChecker{
		"not wired":   {},
		"read failed": {Runtimes: func(context.Context) ([]RuntimeHealth, error) { return nil, errors.New("disk detail") }},
	}
	for name, checker := range cases {
		t.Run(name, func(t *testing.T) {
			findings := checker.Check(t.Context())
			if len(findings) != 1 || findings[0].Code != "interpreter_unknown" || findings[0].Status != health.StatusUnknown {
				t.Fatalf("findings = %+v, want one unknown finding", findings)
			}
			if strings.Contains(findings[0].Detail, "disk detail") {
				t.Errorf("detail leaks the cause: %q", findings[0].Detail)
			}
		})
	}
}

func TestHealthChecksEvaluatesStoreRuntimes(t *testing.T) {
	archive := rigArchive(t, "python")
	manifest := rigManifest(t, map[string][]byte{"python": archive}, []rigRuntime{{id: "python", version: "1.0.0"}})
	checks, err := HealthChecks(t.TempDir(), manifest, 0)
	if err != nil || len(checks) != 1 {
		t.Fatalf("HealthChecks = %v, %v", checks, err)
	}
	if checks[0].ID != ComponentInterpreter || checks[0].Timeout != DefaultVerifyTimeout || checks[0].Remediation != RemediationInstallInterpreter {
		t.Fatalf("check = %+v", checks[0])
	}
	findings := checks[0].Checker.Check(t.Context())
	if len(findings) != 1 || findings[0].Code != "interpreter_runtime_absent" || findings[0].Status != health.StatusDegraded {
		t.Fatalf("findings = %+v", findings)
	}
	rows := healthRuntimes([]interpreter.Verification{{Runtime: "python", Version: "3", State: interpreter.StateInvalid, Reason: interpreter.ReasonProbeFailed}})
	if len(rows) != 1 || rows[0].State != RuntimeStateInvalid || rows[0].Reason != ReasonProbeFailed {
		t.Fatalf("rows = %+v", rows)
	}
}
