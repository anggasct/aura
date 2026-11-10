package media

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	ErrorCodeSizeExceeded ErrorCode = "size_exceeded"
	ErrorCodeIncomplete   ErrorCode = "incomplete"
	ErrorCodeSpoolFailed  ErrorCode = "spool_failed"
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
