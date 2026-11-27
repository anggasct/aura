package operation

import (
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

func newError(code ErrorCode, detail string) error {
	return fault.New(code, detail)
}

func wrapError(code ErrorCode, detail string, cause error) error {
	return fault.Wrap(code, detail, cause)
}

func NewError(code ErrorCode, detail string) error {
	return fault.New(code, detail)
}

func Fail[C fault.Code](code C, cause error) error {
	return fault.Wrap(ErrorCodeFailed, string(code), cause)
}

func nilArgument(what string) error {
	return newError(ErrorCodeInvalidArgument, "operation: "+what+" must not be nil")
}
