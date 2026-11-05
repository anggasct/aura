package approval

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	ErrorCodeInvalidArgument  ErrorCode = "invalid_argument"
	ErrorCodeTokenUnavailable ErrorCode = "token_unavailable"
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

func Errorf(code ErrorCode, format string, args ...any) error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, args...)}
}

func ErrorWrapf(code ErrorCode, cause error, format string, args ...any) error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, args...), Cause: cause}
}
