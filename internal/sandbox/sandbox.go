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

// ChildSentinel is the argv marker that distinguishes a sandbox child
// re-execution from a normal aura invocation.
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

// Spec is the containment contract for one subprocess. Environment and
// working directory are allowlisted. ReadOnlyPaths and ReadWritePaths are
// the only filesystem roots the child may access; Landlock enforces them.
// Network is denied by default: a spec that requests it is refused with
// sandbox_unavailable, and the default case runs the child in an isolated
// network namespace with no external interface.
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

// Run executes command under the containment contract. The child starts
// in its own process group, sees only the allowlisted environment, and is
// confined to WorkingDir. On timeout or cancellation the whole process
// group is killed and reaped, so descendants cannot outlive the parent.
// Output beyond MaxOutputBytes is truncated and reported in Result.
//
// Run is the low-level harness: it enforces process-group, environment,
// working-directory, output, and timeout bounds. It refuses to execute when a
// mandatory kernel primitive is unavailable.
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

// Negotiate reports which host primitives can enforce the containment
// contract, so callers can distinguish "sandbox not available" from a
// runtime violation.
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

// MissingMandatory returns the sorted names of every mandatory containment
// primitive absent from have. It is the single source of truth for the
// fail-closed gate and the status surface, so both report the same names.
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

// Require is the fail-closed gate for an effectful containment capability.
// have is the negotiated host state. A missing mandatory primitive makes
// full kernel-level containment unavailable, so Require returns a
// sandbox_unavailable error naming every absent primitive. The composition
// root must refuse to advertise or execute the capability while it returns
// non-nil; callers reach Run only after Require passes.
func Require(have Primitives) error {
	missing := MissingMandatory(have)
	if len(missing) == 0 {
		return nil
	}
	return Errorf(ErrorCodeSandboxUnavailable, "missing mandatory containment primitive(s): %s", strings.Join(missing, ", "))
}

// CapabilityDependencies reports the host-detected capability dependencies
// for the configuration load path: process containment is available only
// when every mandatory primitive negotiates.
func CapabilityDependencies() capability.Dependencies {
	deps := capability.Dependencies{}
	primitives, err := Negotiate()
	if err == nil && len(MissingMandatory(primitives)) == 0 {
		deps[capability.DependencyProcessContainment] = true
	}
	return deps
}
