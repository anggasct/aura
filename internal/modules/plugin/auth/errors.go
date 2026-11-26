package auth

import (
	"fmt"

	"github.com/anggasct/aura/internal/platform/fault"
)

type ErrorCode string

const (
	ErrRequired    ErrorCode = "plugin_auth_required"
	ErrFailed      ErrorCode = "plugin_auth_failed"
	ErrUnavailable ErrorCode = "plugin_auth_unavailable"
	ErrInvalid     ErrorCode = "plugin_auth_invalid"
	ErrLogin       ErrorCode = "plugin_login_failed"
)

func Errorf(code ErrorCode, format string, args ...any) error {
	return fault.New(code, fmt.Sprintf(format, args...))
}
