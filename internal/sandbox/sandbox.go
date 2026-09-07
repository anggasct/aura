package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/capability"
)

const ChildSentinel = "__aura-sandbox-child"

func IsChild(args []string) bool {
	return len(args) > 1 && args[1] == ChildSentinel
}

type ErrorCode string

const (
	ErrorCodeInvalidArgument         ErrorCode = "invalid_argument"
	ErrorCodeSandboxUnavailable      ErrorCode = "sandbox_unavailable"
	ErrorCodeSandboxViolation        ErrorCode = "sandbox_violation"
	ErrorCodeSandboxInitFailed       ErrorCode = "sandbox_init_failed"
	ErrorCodeSandboxPathDenied       ErrorCode = "sandbox_path_denied"
	ErrorCodeSandboxSyscallDenied    ErrorCode = "sandbox_syscall_denied"
	ErrorCodeSandboxResourceExceeded ErrorCode = "sandbox_resource_exceeded"
	ErrorCodeSandboxTimeout          ErrorCode = "sandbox_timeout"
	ErrorCodeSandboxOutputExceeded   ErrorCode = "sandbox_output_exceeded"
	ErrorCodeSessionClosed           ErrorCode = "session_closed"
	ErrorCodeSessionBroken           ErrorCode = "session_broken"
	ErrorCodeApprovalInvalid         ErrorCode = "approval_invalid"
)

type Error struct {
	Code   ErrorCode
	Detail string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

func CodeOf(err error) (ErrorCode, bool) {
	var target *Error
	if !errors.As(err, &target) {
		return "", false
	}
	return target.Code, true
}

func Errorf(code ErrorCode, format string, args ...any) error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, args...)}
}

type Limits struct {
	MemoryBytes    int64         `json:"memory_bytes"`
	CPUTime        time.Duration `json:"cpu_time"`
	MaxOutputBytes int64         `json:"max_output_bytes"`
	MaxOpenFiles   int64         `json:"max_open_files"`
	MaxProcesses   int64         `json:"max_processes"`
	MaxCoreSize    int64         `json:"max_core_size"`
	FileBytes      int64         `json:"file_bytes"`
	Timeout        time.Duration `json:"timeout"`
}

type Spec struct {
	WorkingDir     string
	ReadOnlyPaths  []string
	ReadWritePaths []string
	AllowEnv       []string
	AllowNetwork   bool
	Limits         Limits
}

func (s *Spec) validate() error {
	if strings.TrimSpace(s.WorkingDir) == "" {
		return Errorf(ErrorCodeInvalidArgument, "working directory must not be empty")
	}
	if s.Limits.Timeout <= 0 {
		return Errorf(ErrorCodeInvalidArgument, "timeout must be positive")
	}
	return nil
}

type Result struct {
	ExitCode   int
	Output     string
	Stdout     string
	Stderr     string
	Terminated bool
	Truncated  bool
}

func Run(ctx context.Context, spec *Spec, command string, args ...string) (Result, error) {
	if spec == nil {
		return Result{}, Errorf(ErrorCodeInvalidArgument, "spec must not be nil")
	}
	if err := spec.validate(); err != nil {
		return Result{}, err
	}
	primitives, err := negotiate()
	if err != nil {
		return Result{}, err
	}
	if err := Require(primitives); err != nil {
		return Result{}, err
	}
	return run(ctx, spec, primitives, command, args...)
}

type Primitives struct {
	UserNamespace bool
	Seccomp       bool
	CgroupV2      bool
	Landlock      bool
	ProcessGroups bool
}

func Negotiate() (Primitives, error) {
	return negotiate()
}

func MissingMandatory(have Primitives) []string {
	var missing []string
	if !have.UserNamespace {
		missing = append(missing, "user_namespace")
	}
	if !have.Landlock {
		missing = append(missing, "landlock")
	}
	if !have.Seccomp {
		missing = append(missing, "seccomp")
	}
	if !have.CgroupV2 {
		missing = append(missing, "cgroup_v2")
	}
	if !have.ProcessGroups {
		missing = append(missing, "process_groups")
	}
	slices.Sort(missing)
	return missing
}

func Require(have Primitives) error {
	missing := MissingMandatory(have)
	if len(missing) == 0 {
		return nil
	}
	return Errorf(ErrorCodeSandboxUnavailable, "missing mandatory containment primitive(s): %s", strings.Join(missing, ", "))
}

func CapabilityDependencies() capability.Dependencies {
	deps := capability.Dependencies{}
	primitives, err := Negotiate()
	if err == nil && len(MissingMandatory(primitives)) == 0 {
		deps[capability.DependencyProcessContainment] = true
	}
	return deps
}
