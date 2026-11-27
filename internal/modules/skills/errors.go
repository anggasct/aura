package skills

import (
	"fmt"

	"github.com/anggasct/aura/internal/platform/fault"
)

type ErrorCode string

const (
	ErrorCodeInvalidArgument    ErrorCode = "invalid_argument"
	ErrorCodeSkillInvalid       ErrorCode = "skill_invalid"
	ErrorCodeSkillNotFound      ErrorCode = "skill_not_found"
	ErrorCodeSkillUnavailable   ErrorCode = "skill_unavailable"
	ErrorCodeSkillDigestChanged ErrorCode = "skill_digest_changed"
)

func Errorf(code ErrorCode, format string, args ...any) error {
	return fault.New(code, fmt.Sprintf(format, args...))
}

func codedError(code ErrorCode, detail string, cause error) error {
	if cause == nil {
		return fault.New(code, detail)
	}
	return fault.Wrap(code, detail, cause)
}

func errNilArgument(name string) error {
	return fault.New(ErrorCodeInvalidArgument, name+" must not be nil")
}
