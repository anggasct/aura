package scheduler

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	ErrorCodeInvalidArgument    ErrorCode = "invalid_argument"
	ErrorCodeExpressionInvalid  ErrorCode = "cron_expression_invalid"
	ErrorCodeTimezoneInvalid    ErrorCode = "cron_timezone_invalid"
	ErrorCodeJobNotFound        ErrorCode = "cron_job_not_found"
	ErrorCodeJobStateInvalid    ErrorCode = "cron_job_state_invalid"
	ErrorCodeOccurrenceConflict ErrorCode = "cron_occurrence_conflict"
	ErrorCodeRuntimeOverloaded  ErrorCode = "cron_runtime_overloaded"
	ErrorCodeDeliveryPending    ErrorCode = "cron_delivery_pending"
	ErrorCodeUnavailable        ErrorCode = "unavailable"
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
