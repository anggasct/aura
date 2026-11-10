package auth

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	ErrRequired    ErrorCode = "plugin_auth_required"
	ErrFailed      ErrorCode = "plugin_auth_failed"
	ErrUnavailable ErrorCode = "plugin_auth_unavailable"
	ErrInvalid     ErrorCode = "plugin_auth_invalid"
	ErrLogin       ErrorCode = "plugin_login_failed"
)

type Error struct {
	Code   ErrorCode
	Detail string
	Err    error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Detail == "" {
		return string(e.Code)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func CodeOf(err error) (ErrorCode, bool) {
	var target *Error
	if errors.As(err, &target) && target != nil {
		return target.Code, true
	}
	return "", false
}

func Errorf(code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, args...)}
}

func Wrap(code ErrorCode, err error, detail string) *Error {
	return &Error{Code: code, Detail: detail, Err: err}
}
