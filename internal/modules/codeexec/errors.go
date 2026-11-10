package codeexec

import (
	"errors"
	"fmt"
	"io/fs"
)

type ErrorCode string

const (
	ErrorCodeInvalidArgument ErrorCode = "invalid_argument"
	ErrorCodeUnavailable     ErrorCode = "runtime_unavailable"
	ErrorCodeRunFailed       ErrorCode = "run_failed"
	ErrorCodeArtifactQuota   ErrorCode = "artifact_quota"
)

type Error struct {
	Code   ErrorCode
	Detail string
	cause  error
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

func (e *Error) Unwrap() error {
	return e.cause
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

func wrapf(code ErrorCode, cause error, format string, args ...any) error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, args...), cause: cause}
}

func pathFree(err error) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err
	}
	return err
}
