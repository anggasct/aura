package health

import (
	"context"
	"fmt"
	"time"
)

const (
	ComponentInterpreter = "interpreter"

	RemediationInstallInterpreter = "install-interpreter-runtime"

	InterpreterRuntimeInstalled = "installed"
	InterpreterRuntimeAbsent    = "absent"
	InterpreterRuntimeInvalid   = "invalid"
	InterpreterRuntimePending   = "pending"
)

type InterpreterRuntime struct {
	Runtime string
	Version string
	State   string
	Reason  string
}

type InterpreterChecker struct {
	Runtimes func(ctx context.Context) ([]InterpreterRuntime, error)
}

func (c InterpreterChecker) Check(ctx context.Context) []Finding {
	now := time.Now().UTC()
	if c.Runtimes == nil {
		return []Finding{{Component: ComponentInterpreter, Code: "interpreter_unknown", Status: StatusUnknown, Detail: "interpreter runtime state is not wired", CheckedAt: now}}
	}
	runtimes, err := c.Runtimes(ctx)
	if err != nil {
		return []Finding{{Component: ComponentInterpreter, Code: "interpreter_unknown", Status: StatusUnknown, Detail: "interpreter runtime state unavailable", CheckedAt: now}}
	}
	findings := make([]Finding, 0, len(runtimes))
	for i := range runtimes {
		findings = append(findings, interpreterRuntimeFinding(&runtimes[i], now))
	}
	return findings
}

func interpreterRuntimeFinding(runtime *InterpreterRuntime, now time.Time) Finding {
	label := runtime.Runtime + " " + runtime.Version
	code, status, detail := "ok", StatusUp, label+" is installed and its files match the recorded digest"
	remediation := RemediationNone
	switch runtime.State {
	case InterpreterRuntimeInstalled:
	case InterpreterRuntimeAbsent:
		code, status, remediation = "interpreter_runtime_absent", StatusDegraded, RemediationInstallInterpreter
		detail = fmt.Sprintf("%s is not installed; interpreter runs for it are refused until aura interpreter install %s runs", label, runtime.Runtime)
	case InterpreterRuntimeInvalid:
		code, status, remediation = "interpreter_runtime_invalid", StatusDegraded, RemediationInstallInterpreter
		detail = fmt.Sprintf("%s failed verification (%s); interpreter runs for it are refused until it is removed and installed again", label, runtime.Reason)
	default:
		code, status = "interpreter_runtime_pending", StatusUnknown
		detail = label + " has not been verified yet"
	}
	return Finding{
		ID: ComponentInterpreter + "/" + code + "/" + runtime.Runtime, Component: ComponentInterpreter, Scope: runtime.Runtime,
		Code: code, Status: status, Detail: detail, Remediation: remediation, CheckedAt: now,
	}
}
