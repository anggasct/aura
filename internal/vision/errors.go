package vision

import (
	"errors"
	"fmt"
	"strings"
)

type ErrorCode string

const (
	ErrorCodeInvalidArgument           ErrorCode = "invalid_argument"
	ErrorCodeVisionInvalid             ErrorCode = "vision_invalid"
	ErrorCodeVisionDecodeFailed        ErrorCode = "vision_decode_failed"
	ErrorCodeVisionLimitExceeded       ErrorCode = "vision_limit_exceeded"
	ErrorCodeVisionFormatUnsupported   ErrorCode = "vision_format_unsupported"
	ErrorCodeVisionBudgetExceeded      ErrorCode = "vision_budget_exceeded"
	ErrorCodeVisionArtifactUnavailable ErrorCode = "vision_artifact_unavailable"
	ErrorCodeArtifactQuotaExceeded     ErrorCode = "artifact_quota_exceeded"
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

func wrapWithCode(code ErrorCode, detail string, cause error) error {
	if cause == nil {
		return &Error{Code: code, Detail: detail}
	}
	return fmt.Errorf("%w: %w", &Error{Code: code, Detail: detail}, cause)
}

func isQuotaExceeded(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), string(ErrorCodeArtifactQuotaExceeded))
}

func errNilArgument(name string) error {
	return &Error{Code: ErrorCodeInvalidArgument, Detail: name + " must not be nil"}
}
