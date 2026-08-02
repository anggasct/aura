package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

type ErrorCode string

const (
	ErrorCodeInvalidArgument    ErrorCode = "invalid_argument"
	ErrorCodeSandboxUnavailable ErrorCode = "sandbox_unavailable"
	ErrorCodeSandboxViolation   ErrorCode = "sandbox_violation"
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

// Limits are the resource bounds a contained process must stay inside.
// Timeout and MaxOutputBytes are enforced by this harness. The remaining
// fields are contract declarations for the full sandbox (feat-sandbox-runner)
// and are not yet enforced here.
type Limits struct {
	MaxOutputBytes int64
	MaxOpenFiles   int64
	MaxProcesses   int64
	MaxCoreSize    int64
	Timeout        time.Duration
}

// Spec is the containment contract for one subprocess. Environment and
// working directory are allowlisted. Network is denied by default: a spec
// that requests it is refused with sandbox_unavailable; kernel-level denial
// of undeclared filesystem and syscall access is delegated to the full
// sandbox and reported through Negotiate.
type Spec struct {
	WorkingDir   string
	AllowEnv     []string
	AllowNetwork bool
	Limits       Limits
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

// Result reports what a contained run did, so callers never have to guess
// whether termination was enforced.
type Result struct {
	ExitCode   int
	Output     string
	Terminated bool
	Truncated  bool
}

// Run executes command under the containment contract. The child starts
// in its own process group, sees only the allowlisted environment, and is
// confined to WorkingDir. On timeout or cancellation the whole process
// group is killed and reaped, so descendants cannot outlive the parent.
// Output beyond MaxOutputBytes is truncated and reported in Result.
// Run consults Negotiate and fails closed with sandbox_unavailable when the
// host cannot provide the containment primitives; callers that need
// kernel-level filesystem or syscall denial must check Negotiate before
// running, since the full sandbox enforcement lands with feat-sandbox-runner.
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
	return run(ctx, spec, primitives, command, args...)
}

// Negotiate reports which host primitives can enforce the containment
// contract, so callers can distinguish "sandbox not available" from a
// runtime violation.
type Primitives struct {
	Seccomp       bool
	CgroupV2      bool
	Landlock      bool
	ProcessGroups bool
}

func Negotiate() (Primitives, error) {
	return negotiate()
}
