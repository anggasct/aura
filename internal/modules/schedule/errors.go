package schedule

import (
	"fmt"

	"github.com/anggasct/aura/internal/platform/fault"
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

func Errorf(code ErrorCode, format string, args ...any) error {
	return fault.New(code, fmt.Sprintf(format, args...))
}
