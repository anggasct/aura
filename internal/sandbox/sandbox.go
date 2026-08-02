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
// Every field is enforced; zero means the kernel default, never unlimited
// for the fields the harness can bound.
type Limits struct {
	MaxOutputBytes int64
	MaxOpenFiles   int64
	MaxProcesses   int64
	MaxCoreSize    int64
	Timeout        time.Duration
}

// Spec is the containment contract for one subprocess. Environment and
// file descriptors are allowlisted; network is denied by default and can
// only be requested explicitly, and only when the host primitives that
// enforce it are available.
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
}

// Run executes command under the containment contract. The child starts
// in its own process group, sees only the allowlisted environment, and is
// confined to WorkingDir. On timeout or cancellation the whole process
// group is killed and reaped, so descendants cannot outlive the parent.
// Run fails closed: if the host cannot enforce a required primitive it
// returns sandbox_unavailable and never launches the child.
func Run(ctx context.Context, spec *Spec, command string, args ...string) (Result, error) {
	return run(ctx, spec, command, args...)
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
