package connector

import (
	"errors"
	"fmt"
	"slices"

	"github.com/anggasct/aura/internal/plugin/auth"
)

type ErrorCode string

const (
	ErrBackendUnavailable ErrorCode = "plugin_backend_unavailable"
	ErrRPCInvalid         ErrorCode = "plugin_rpc_invalid"
	ErrRPCLimit           ErrorCode = "plugin_rpc_limit"
	ErrTierViolation      ErrorCode = "plugin_tier_violation"
	ErrConnector          ErrorCode = "plugin_connector_error"
	ErrRequestFailed      ErrorCode = "plugin_request_failed"
	ErrConfigInvalid      ErrorCode = "plugin_connector_config_invalid"
	ErrAuthRequired       ErrorCode = ErrorCode(auth.ErrRequired)
	ErrAuthFailed         ErrorCode = ErrorCode(auth.ErrFailed)
	ErrAuthUnavailable    ErrorCode = ErrorCode(auth.ErrUnavailable)
)

type Error struct {
	Code        ErrorCode
	Detail      string
	Reported    string
	RequestSent bool
	Err         error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	text := string(e.Code)
	if e.Reported != "" {
		text += ": " + e.Reported
	}
	if e.Detail != "" {
		text += ": " + e.Detail
	}
	return text
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *Error) Public() string {
	if e.Reported != "" {
		return string(e.Code) + ": " + e.Reported
	}
	return string(e.Code)
}

func CodeOf(err error) (ErrorCode, bool) {
	var target *Error
	if errors.As(err, &target) && target != nil {
		return target.Code, true
	}
	return "", false
}

func Errorf(code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, args...)}
}

func Wrap(code ErrorCode, err error, detail string) *Error {
	return &Error{Code: code, Detail: detail, Err: err}
}

func authCode(err error) (ErrorCode, bool) {
	code, ok := auth.CodeOf(err)
	if !ok {
		return "", false
	}
	if known := ErrorCode(code); slices.Contains([]ErrorCode{ErrAuthRequired, ErrAuthFailed, ErrAuthUnavailable}, known) {
		return known, true
	}
	return "", false
}
