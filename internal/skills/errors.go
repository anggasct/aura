package skills

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	ErrorCodeInvalidArgument  ErrorCode = "invalid_argument"
	ErrorCodeSkillInvalid     ErrorCode = "skill_invalid"
	ErrorCodeSkillNotFound    ErrorCode = "skill_not_found"
	ErrorCodeSkillUnavailable ErrorCode = "skill_unavailable"
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

func codedError(code ErrorCode, detail string, cause error) error {
	if cause == nil {
		return &Error{Code: code, Detail: detail}
	}
	return fmt.Errorf("%w: %w", &Error{Code: code, Detail: detail}, cause)
}

func errNilArgument(name string) error {
	return &Error{Code: ErrorCodeInvalidArgument, Detail: name + " must not be nil"}
}
