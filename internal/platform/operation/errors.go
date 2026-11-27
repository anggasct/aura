package operation

import (
	"context"
	"errors"

	"github.com/anggasct/aura/internal/platform/fault"
)

type ErrorCode string

const (
	ErrorCodeInvalidArgument ErrorCode = "invalid_argument"
	ErrorCodeInvalid         ErrorCode = "operation_invalid"
	ErrorCodeUnknown         ErrorCode = "operation_unknown"
	ErrorCodeInputInvalid    ErrorCode = "operation_input_invalid"
	ErrorCodeNotExposed      ErrorCode = "operation_not_exposed"
	ErrorCodeDenied          ErrorCode = "operation_denied"
	ErrorCodePreviewStale    ErrorCode = "operation_preview_stale"
	ErrorCodeUncommitted     ErrorCode = "operation_uncommitted"
	ErrorCodeKeyReused       ErrorCode = "operation_key_reused"
	ErrorCodeNotCommitted    ErrorCode = "operation_not_committed"
	ErrorCodeRateLimited     ErrorCode = "operation_rate_limited"
	ErrorCodeFailed          ErrorCode = "operation_failed"
)

func NewError(code ErrorCode, detail string) error {
	return fault.New(code, detail)
}

func wrapError(code ErrorCode, detail string, cause error) error {
	return fault.Wrap(code, detail, cause)
}

func Fail[C fault.Code](code C, cause error) error {
	return fault.Wrap(ErrorCodeFailed, string(code), cause)
}

type notPerformedError struct {
	code  string
	cause error
}

func (e *notPerformedError) Error() string { return e.cause.Error() }

func (e *notPerformedError) Unwrap() error { return e.cause }

func NotPerformed[C fault.Code](code C, cause error) error {
	var coded error
	if own, ok := any(code).(ErrorCode); ok {
		coded = fault.Wrap(own, "", cause)
	} else {
		coded = Fail(code, cause)
	}
	return &notPerformedError{code: string(code), cause: coded}
}

func definitiveRefusal(err error) (string, bool) {
	var refusal *notPerformedError
	if errors.As(err, &refusal) {
		return refusal.code, true
	}
	return "", false
}

func interrupted(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func nilArgument(what string) error {
	return NewError(ErrorCodeInvalidArgument, "operation: "+what+" must not be nil")
}
