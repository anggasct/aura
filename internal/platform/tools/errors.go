package tools

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	ErrorCodeDisplayTooLarge    ErrorCode = "approval_display_too_large"
	ErrorCodeDisplayRedacted    ErrorCode = "approval_display_redacted"
	ErrorCodeDisplayInvalid     ErrorCode = "tool_display_invalid"
	ErrorCodeArgumentsAmbiguous ErrorCode = "arguments_ambiguous"
)

type Error struct {
	Code   ErrorCode
	Detail string
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

func CodeOf(err error) (ErrorCode, bool) {
	var target *Error
	if errors.As(err, &target) && target != nil {
		return target.Code, true
	}
	return "", false
}

func errorf(code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, args...)}
}
