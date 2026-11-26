package child

import (
	"fmt"

	"github.com/anggasct/aura/internal/platform/fault"
)

type ErrorCode string

const (
	ErrorCodeInvalidArgument      ErrorCode = "invalid_argument"
	ErrorCodeChildInvalid         ErrorCode = "child_invalid"
	ErrorCodeChildNotFound        ErrorCode = "child_not_found"
	ErrorCodeChildConflict        ErrorCode = "child_conflict"
	ErrorCodeChildUnavailable     ErrorCode = "child_unavailable"
	ErrorCodeChildDepthExceeded   ErrorCode = "child_depth_exceeded"
	ErrorCodeChildSpawnDenied     ErrorCode = "child_spawn_forbidden"
	ErrorCodeChildForbidden       ErrorCode = "child_forbidden"
	ErrorCodeBudgetExceeded       ErrorCode = "child_budget_exceeded"
	ErrorCodeRuntimeOverloaded    ErrorCode = "runtime_overloaded"
	ErrorCodeTurnDeadlineExceeded ErrorCode = "turn_deadline_exceeded"
	ErrorCodeTurnCancelled        ErrorCode = "turn_cancelled"
)

func Errorf(code ErrorCode, format string, args ...any) error {
	return fault.New(code, fmt.Sprintf(format, args...))
}

func errNilArgument(name string) error {
	return fault.New(ErrorCodeInvalidArgument, name+" must not be nil")
}
