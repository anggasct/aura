package tools

import (
	"fmt"

	"github.com/anggasct/aura/internal/platform/fault"
)

type ErrorCode string

const (
	ErrorCodeDisplayTooLarge    ErrorCode = "approval_display_too_large"
	ErrorCodeDisplayRedacted    ErrorCode = "approval_display_redacted"
	ErrorCodeDisplayInvalid     ErrorCode = "tool_display_invalid"
	ErrorCodeArgumentsAmbiguous ErrorCode = "arguments_ambiguous"
)

func errorf(code ErrorCode, format string, args ...any) error {
	return fault.New(code, fmt.Sprintf(format, args...))
}
