package vision

import (
	"fmt"

	"github.com/anggasct/aura/internal/platform/fault"
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

func Errorf(code ErrorCode, format string, args ...any) error {
	return fault.New(code, fmt.Sprintf(format, args...))
}

func wrapWithCode(code ErrorCode, detail string, cause error) error {
	return fault.Wrap(code, detail, cause)
}

func isQuotaExceeded(err error) bool {
	if err == nil {
		return false
	}
	return fault.HasCode(err, ErrorCodeArtifactQuotaExceeded)
}

func errNilArgument(name string) error {
	return fault.New(ErrorCodeInvalidArgument, name+" must not be nil")
}
