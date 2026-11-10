package codeexec

import (
	"syscall"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/sandbox"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

func TestClassifyResult(t *testing.T) {
	const limit = 10 * time.Second
	cases := []struct {
		name   string
		result sandbox.Result
		want   ResultClass
	}{
		{"clean exit", sandbox.Result{}, ClassOK},
		{"non-zero exit", sandbox.Result{ExitCode: 3}, ClassExitNonzero},
		{"timeout", sandbox.Result{Terminated: true, TimedOut: true, ExitCode: -1, Signal: int(syscall.SIGKILL)}, ClassTimeout},
		{"timeout beats the disk verdict", sandbox.Result{Terminated: true, TimedOut: true, DiskLimited: true}, ClassTimeout},
		{"cancellation", sandbox.Result{Terminated: true, ExitCode: -1, Signal: int(syscall.SIGKILL)}, ClassCancelled},
		{"seccomp kill", sandbox.Result{ExitCode: -1, Signal: int(syscall.SIGSYS)}, ClassPolicyDenied},
		{"disk limit kills the tree", sandbox.Result{Terminated: true, DiskLimited: true, ExitCode: -1, Signal: int(syscall.SIGKILL)}, ClassDiskLimit},
		{"disk limit seen after a normal exit", sandbox.Result{DiskLimited: true}, ClassDiskLimit},
		{"oom kill", sandbox.Result{OOMKilled: true, ExitCode: -1, Signal: int(syscall.SIGKILL)}, ClassMemoryLimit},
		{"oom kill beats the cpu verdict", sandbox.Result{OOMKilled: true, ExitCode: -1, Signal: int(syscall.SIGKILL), CPUTime: 2 * limit}, ClassMemoryLimit},
		{"cpu time reached the limit", sandbox.Result{ExitCode: -1, Signal: int(syscall.SIGKILL), CPUTime: limit + time.Second}, ClassCPULimit},
		{"cpu signal after the limit", sandbox.Result{ExitCode: -1, Signal: int(syscall.SIGXCPU), CPUTime: limit}, ClassCPULimit},
		{"sigkill before the cpu limit is a crash", sandbox.Result{ExitCode: -1, Signal: int(syscall.SIGKILL), CPUTime: limit - time.Second}, ClassRuntimeCrashed},
		{"cpu signal before the limit is a crash", sandbox.Result{ExitCode: -1, Signal: int(syscall.SIGXCPU), CPUTime: time.Second}, ClassRuntimeCrashed},
		{"file limit with a failed exit", sandbox.Result{FileLimited: true, ExitCode: 1}, ClassFileLimit},
		{"file limit with a signal", sandbox.Result{FileLimited: true, ExitCode: -1, Signal: int(syscall.SIGXFSZ)}, ClassFileLimit},
		{"a file at the limit in a clean run", sandbox.Result{FileLimited: true}, ClassOK},
		{"process limit with a failed exit", sandbox.Result{PidsLimited: true, ExitCode: 1}, ClassProcessLimit},
		{"process limit in a clean run", sandbox.Result{PidsLimited: true}, ClassOK},
		{"segmentation fault", sandbox.Result{ExitCode: -1, Signal: int(syscall.SIGSEGV)}, ClassRuntimeCrashed},
		{"abort", sandbox.Result{ExitCode: -1, Signal: int(syscall.SIGABRT)}, ClassRuntimeCrashed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(&tc.result, limit); got != tc.want {
				t.Fatalf("classify = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestClassifyWithoutACPULimitNeverReportsCPU(t *testing.T) {
	result := sandbox.Result{ExitCode: -1, Signal: int(syscall.SIGKILL), CPUTime: time.Hour}
	if got := classify(&result, 0); got != ClassRuntimeCrashed {
		t.Fatalf("classify = %s, want %s", got, ClassRuntimeCrashed)
	}
}

func TestResultClassMapsToTheBrokerClass(t *testing.T) {
	cases := map[ResultClass]toolbroker.ResultClass{
		ClassOK:             toolbroker.ResultOK,
		ClassTimeout:        toolbroker.ResultDeadlineExceeded,
		ClassCancelled:      toolbroker.ResultDeadlineExceeded,
		ClassPolicyDenied:   toolbroker.ResultPolicyDenied,
		ClassExitNonzero:    toolbroker.ResultExecutionFailed,
		ClassCPULimit:       toolbroker.ResultExecutionFailed,
		ClassMemoryLimit:    toolbroker.ResultExecutionFailed,
		ClassProcessLimit:   toolbroker.ResultExecutionFailed,
		ClassFileLimit:      toolbroker.ResultExecutionFailed,
		ClassDiskLimit:      toolbroker.ResultExecutionFailed,
		ClassRuntimeCrashed: toolbroker.ResultExecutionFailed,
	}
	for class, want := range cases {
		t.Run(string(class), func(t *testing.T) {
			if got := class.toolClass(); got != want {
				t.Fatalf("toolClass = %s, want %s", got, want)
			}
		})
	}
}
