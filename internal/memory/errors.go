package memory

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	ErrorCodeInvalidArgument            ErrorCode = "invalid_argument"
	ErrorCodeQueryInvalid               ErrorCode = "memory_query_invalid"
	ErrorCodeBudgetInvalid              ErrorCode = "memory_budget_invalid"
	ErrorCodeProjectionStale            ErrorCode = "memory_projection_stale"
	ErrorCodeProjectionCorrupt          ErrorCode = "memory_projection_corrupt"
	ErrorCodeModelCapabilityUnsupported ErrorCode = "memory_model_capability_unsupported"
	ErrorCodeRebuildRequired            ErrorCode = "memory_rebuild_required"
	ErrorCodeUnavailable                ErrorCode = "unavailable"
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
