package config

import (
	"errors"
	"fmt"
)

type ErrorCode string

const ErrorCodeVersionUnsupported ErrorCode = "config_version_unsupported"

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
