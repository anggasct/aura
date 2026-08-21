package harness

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	ErrorCodeInvalidArgument       ErrorCode = "invalid_argument"
	ErrorCodePhaseInvalid          ErrorCode = "phase_invalid"
	ErrorCodeLifecycleInvalid      ErrorCode = "lifecycle_invalid"
	ErrorCodeDurabilityFailed      ErrorCode = "durability_failed"
	ErrorCodeProviderFailed        ErrorCode = "provider_failed"
	ErrorCodeCancelled             ErrorCode = "cancelled"
	ErrorCodeLivePublishFailed     ErrorCode = "live_publish_failed"
	ErrorCodeCatalogInvalid        ErrorCode = "catalog_invalid"
	ErrorCodeCapabilityUnavailable ErrorCode = "capability_unavailable"
	ErrorCodeProviderUnavailable   ErrorCode = "provider_unavailable"
	ErrorCodeResultTooLarge        ErrorCode = "result_too_large"
	ErrorCodeShutdownTimeout       ErrorCode = "shutdown_timeout"
)

type Error struct {
	Code   ErrorCode
	Detail string
	Cause  error
}

func (e *Error) Error() string {
	if e.Cause == nil {
		return fmt.Sprintf("%s: %s", e.Code, e.Detail)
	}
	return fmt.Sprintf("%s: %s: %v", e.Code, e.Detail, e.Cause)
}

func (e *Error) Unwrap() error {
	return e.Cause
}

func CodeOf(err error) (ErrorCode, bool) {
	var target *Error
	if !errors.As(err, &target) {
		return "", false
	}
	return target.Code, true
}

func codedError(code ErrorCode, detail string, cause error) error {
	return &Error{Code: code, Detail: detail, Cause: cause}
}

func invalidArgument(detail string) error {
	return codedError(ErrorCodeInvalidArgument, detail, nil)
}
