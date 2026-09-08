package restate

import "errors"

type ErrorCode string

const (
	ErrorCodeDisabled           ErrorCode = "durable_disabled"
	ErrorCodeBinaryMissing      ErrorCode = "durable_binary_missing"
	ErrorCodeUnreachable        ErrorCode = "durable_unreachable"
	ErrorCodeRegistrationFailed ErrorCode = "durable_registration_failed"
)

type Error struct {
	Code   ErrorCode
	Detail string
}

func (e *Error) Error() string {
	return string(e.Code) + ": " + e.Detail
}

func CodeOf(err error) (ErrorCode, bool) {
	var target *Error
	if errors.As(err, &target) {
		return target.Code, true
	}
	return "", false
}
