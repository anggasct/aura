package webhook

import (
	"fmt"

	"github.com/anggasct/aura/internal/platform/fault"
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
	ErrorCodeExecutionConflict   ErrorCode = "webhook_execution_conflict"
	ErrorCodeExecutionInvalid    ErrorCode = "webhook_execution_invalid"
)

func Errorf(code ErrorCode, format string, args ...any) error {
	return fault.New(code, fmt.Sprintf(format, args...))
}
