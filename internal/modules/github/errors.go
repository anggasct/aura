package github

import (
	"fmt"

	"github.com/anggasct/aura/internal/platform/fault"
)

type ErrorCode string

const (
	ErrorCodeInvalidArgument      ErrorCode = "invalid_argument"
	ErrorCodeCorrelationUnmatched ErrorCode = "github_correlation_unmatched"
	ErrorCodeCorrelationConflict  ErrorCode = "github_correlation_conflict"
)

func Errorf(code ErrorCode, format string, args ...any) error {
	return fault.New(code, fmt.Sprintf(format, args...))
}
