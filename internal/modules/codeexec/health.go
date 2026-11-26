package codeexec

import (
	"context"
	"fmt"
	"time"

	"github.com/anggasct/aura/internal/modules/codeexec/interpreter"
	"github.com/anggasct/aura/internal/platform/health"
)

const (
	ComponentInterpreter = "interpreter"

	RemediationInstallInterpreter = "install-interpreter-runtime"

	RuntimeStateInstalled = "installed"
	RuntimeStateAbsent    = "absent"
	RuntimeStateInvalid   = "invalid"
	RuntimeStatePending   = "pending"

	ReasonProbeFailed = "probe_failed"

	DefaultVerifyTimeout = time.Minute
)

type RuntimeHealth struct {
	Runtime string
	Version string
	State   string
	Reason  string
}

type HealthChecker struct {
	Runtimes func(ctx context.Context) ([]RuntimeHealth, error)
}

func (c HealthChecker) Check(ctx context.Context) []health.Finding {
	now := time.Now().UTC()
	if c.Runtimes == nil {
		return []health.Finding{{Component: ComponentInterpreter, Code: "interpreter_unknown", Status: health.StatusUnknown, Detail: "interpreter runtime state is not wired", CheckedAt: now}}
	}
	runtimes, err := c.Runtimes(ctx)
	if err != nil {
		return []health.Finding{{Component: ComponentInterpreter, Code: "interpreter_unknown", Status: health.StatusUnknown, Detail: "interpreter runtime state unavailable", CheckedAt: now}}
	}
	findings := make([]health.Finding, 0, len(runtimes))
	for i := range runtimes {
		findings = append(findings, interpreterRuntimeFinding(&runtimes[i], now))
	}
	return findings
}

func HealthChecks(dataRoot string, manifest *interpreter.Manifest, timeout time.Duration) ([]health.RegisteredCheck, error) {
	if manifest == nil {
		var err error
		manifest, err = interpreter.DefaultManifest()
		if err != nil {
			return nil, err
		}
	}
	runtimeStore, err := interpreter.NewStore(dataRoot, manifest)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = DefaultVerifyTimeout
	}
	return []health.RegisteredCheck{{
		ID: ComponentInterpreter,
		Checker: HealthChecker{Runtimes: func(ctx context.Context) ([]RuntimeHealth, error) {
			verifications, err := runtimeStore.List(ctx)
			if err != nil {
				return nil, err
			}
			return healthRuntimes(verifications), nil
		}},
		Timeout:     timeout,
		Remediation: RemediationInstallInterpreter,
	}}, nil
}

func healthRuntimes(verifications []interpreter.Verification) []RuntimeHealth {
	out := make([]RuntimeHealth, 0, len(verifications))
	for _, verification := range verifications {
		out = append(out, RuntimeHealth{
			Runtime: verification.Runtime,
			Version: verification.Version,
			State:   string(verification.State),
			Reason:  string(verification.Reason),
		})
	}
	return out
}

func interpreterRuntimeFinding(runtime *RuntimeHealth, now time.Time) health.Finding {
	label := runtime.Runtime + " " + runtime.Version
	code, status, detail := "ok", health.StatusUp, label+" is installed and its files match the recorded digest"
	remediation := health.RemediationNone
	switch runtime.State {
	case RuntimeStateInstalled:
	case RuntimeStateAbsent:
		code, status, remediation = "interpreter_runtime_absent", health.StatusDegraded, RemediationInstallInterpreter
		detail = fmt.Sprintf("%s is not installed; interpreter runs for it are refused until aura interpreter install %s runs", label, runtime.Runtime)
	case RuntimeStateInvalid:
		if runtime.Reason == ReasonProbeFailed {
			code, status = "interpreter_runtime_probe_failed", health.StatusDegraded
			detail = label + " is installed and its files match, but its version probe failed in the sandbox; interpreter runs for it are refused"
			break
		}
		code, status, remediation = "interpreter_runtime_invalid", health.StatusDegraded, RemediationInstallInterpreter
		detail = fmt.Sprintf("%s failed verification (%s); interpreter runs for it are refused until it is removed and installed again", label, runtime.Reason)
	default:
		code, status = "interpreter_runtime_pending", health.StatusUnknown
		detail = label + " has not been verified yet"
	}
	return health.Finding{
		ID: ComponentInterpreter + "/" + code + "/" + runtime.Runtime, Component: ComponentInterpreter, Scope: runtime.Runtime,
		Code: code, Status: status, Detail: detail, Remediation: remediation, CheckedAt: now,
	}
}
