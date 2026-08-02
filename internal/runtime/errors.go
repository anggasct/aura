package runtime

import (
	"errors"
	"fmt"
)

// ErrorCode is a stable runtime error code. Callers branch on the code, never
// on the message; retryability is a property of the code.
type ErrorCode string

const (
	ErrorCodeRuntimeOverloaded          ErrorCode = "runtime_overloaded"
	ErrorCodeTurnDeadlineExceeded       ErrorCode = "turn_deadline_exceeded"
	ErrorCodeTurnCancelled              ErrorCode = "turn_cancelled"
	ErrorCodeModelCapabilityUnsupported ErrorCode = "model_capability_unsupported"
	ErrorCodePolicyDenied               ErrorCode = "policy_denied"
	ErrorCodeStorageUnavailable         ErrorCode = "storage_unavailable"
	ErrorCodeBudgetExhausted            ErrorCode = "budget_exhausted"
	ErrorCodeRuntimeInternal            ErrorCode = "runtime_internal"
	ErrorCodeInvalidArgument            ErrorCode = "invalid_argument"
)

// Retryable reports whether a caller may resubmit the same work after this
// code. A conditional code is reported non-retryable here; the caller decides
// based on its own state (for example a deadline may be retried with a fresh
// deadline).
func (c ErrorCode) Retryable() bool {
	switch c {
	case ErrorCodeRuntimeOverloaded, ErrorCodeStorageUnavailable:
		return true
	default:
		return false
	}
}

type Error struct {
	Code   ErrorCode
	Detail string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

// CodeOf extracts the stable runtime code from err, or reports absent.
func CodeOf(err error) (ErrorCode, bool) {
	var target *Error
	if !errors.As(err, &target) {
		return "", false
	}
	return target.Code, true
}

func codedError(code ErrorCode, detail string, cause error) error {
	if cause == nil {
		return &Error{Code: code, Detail: detail}
	}
	return fmt.Errorf("%w: %w", &Error{Code: code, Detail: detail}, cause)
}

func invalidArgument(detail string) error {
	return &Error{Code: ErrorCodeInvalidArgument, Detail: detail}
}
