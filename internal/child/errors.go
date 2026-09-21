package child

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	ErrorCodeInvalidArgument    ErrorCode = "invalid_argument"
	ErrorCodeChildInvalid       ErrorCode = "child_invalid"
	ErrorCodeChildNotFound      ErrorCode = "child_not_found"
	ErrorCodeChildConflict      ErrorCode = "child_conflict"
	ErrorCodeChildUnavailable   ErrorCode = "child_unavailable"
	ErrorCodeChildDepthExceeded ErrorCode = "child_depth_exceeded"
	ErrorCodeChildSpawnDenied   ErrorCode = "child_spawn_forbidden"
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

func errNilArgument(name string) error {
	return &Error{Code: ErrorCodeInvalidArgument, Detail: name + " must not be nil"}
}
