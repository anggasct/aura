package codeexec

import (
	"syscall"
	"time"

	"github.com/anggasct/aura/internal/sandbox"
	"github.com/anggasct/aura/internal/toolbroker"
)

type ResultClass string

const (
	ClassOK             ResultClass = "ok"
	ClassExitNonzero    ResultClass = "exit_nonzero"
	ClassTimeout        ResultClass = "timeout"
	ClassCancelled      ResultClass = "cancelled"
	ClassCPULimit       ResultClass = "cpu_limit"
	ClassMemoryLimit    ResultClass = "memory_limit"
	ClassProcessLimit   ResultClass = "process_limit"
	ClassFileLimit      ResultClass = "file_limit"
	ClassDiskLimit      ResultClass = "disk_limit"
	ClassPolicyDenied   ResultClass = "policy_denied"
	ClassRuntimeCrashed ResultClass = "runtime_crashed"
)

func classify(result *sandbox.Result, cpuLimit time.Duration) ResultClass {
	failed := result.ExitCode != 0 || result.Signal != 0
	switch {
	case result.TimedOut:
		return ClassTimeout
	case result.Terminated && !result.DiskLimited:
		return ClassCancelled
	case result.Signal == int(syscall.SIGSYS):
		return ClassPolicyDenied
	case result.DiskLimited:
		return ClassDiskLimit
	case result.OOMKilled:
		return ClassMemoryLimit
	case cpuExhausted(result, cpuLimit):
		return ClassCPULimit
	case result.FileLimited && failed:
		return ClassFileLimit
	case result.PidsLimited && failed:
		return ClassProcessLimit
	case result.Signal != 0:
		return ClassRuntimeCrashed
	case result.ExitCode != 0:
		return ClassExitNonzero
	default:
		return ClassOK
	}
}

func cpuExhausted(result *sandbox.Result, limit time.Duration) bool {
	if result.Signal != int(syscall.SIGKILL) && result.Signal != int(syscall.SIGXCPU) {
		return false
	}
	return limit > 0 && result.CPUTime >= limit
}

func (c ResultClass) toolClass() toolbroker.ResultClass {
	switch c {
	case ClassOK:
		return toolbroker.ResultOK
	case ClassTimeout, ClassCancelled:
		return toolbroker.ResultDeadlineExceeded
	case ClassPolicyDenied:
		return toolbroker.ResultPolicyDenied
	default:
		return toolbroker.ResultExecutionFailed
	}
}
