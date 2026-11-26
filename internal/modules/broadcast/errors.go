package broadcast

import (
	"fmt"

	"github.com/anggasct/aura/internal/platform/fault"
)

type ErrorCode string

const (
	ErrorCodeInvalidArgument ErrorCode = "invalid_argument"
	ErrorCodeUnknownAlias    ErrorCode = "unknown_alias"
	ErrorCodeChannelUnknown  ErrorCode = "channel_unknown"
	ErrorCodeConflict        ErrorCode = "conflict"
	ErrorCodeNotFound        ErrorCode = "broadcast_not_found"
	ErrorCodeUnavailable     ErrorCode = "unavailable"
)

func Errorf(code ErrorCode, format string, args ...any) error {
	return fault.New(code, fmt.Sprintf(format, args...))
}
