package webhook

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	ErrorCodeInvalidRequest      ErrorCode = "webhook_invalid_request"
	ErrorCodeAuthFailed          ErrorCode = "webhook_auth_failed"
	ErrorCodeReplayConflict      ErrorCode = "webhook_replay_conflict"
	ErrorCodeBodyTooLarge        ErrorCode = "webhook_body_too_large"
	ErrorCodeMediaUnsupported    ErrorCode = "webhook_media_type_unsupported"
	ErrorCodeRateLimited         ErrorCode = "webhook_rate_limited"
	ErrorCodeRuntimeOverloaded   ErrorCode = "runtime_overloaded"
	ErrorCodeExecutionNotFound   ErrorCode = "execution_not_found"
	ErrorCodeInvalidArgument     ErrorCode = "webhook_invalid_argument"
	ErrorCodeKeyResolutionFailed ErrorCode = "webhook_key_resolution_failed"
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
