package connector

import (
	"fmt"
	"slices"

	"github.com/anggasct/aura/internal/modules/plugin/auth"
	"github.com/anggasct/aura/internal/platform/fault"
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
	ErrEffectNotPerformed ErrorCode = "plugin_effect_not_performed"
	ErrToolDrift          ErrorCode = "plugin_tool_drift"
	ErrAuthRequired       ErrorCode = ErrorCode(auth.ErrRequired)
	ErrAuthFailed         ErrorCode = ErrorCode(auth.ErrFailed)
	ErrAuthUnavailable    ErrorCode = ErrorCode(auth.ErrUnavailable)
)

type callFailure struct {
	code        ErrorCode
	detail      string
	reported    string
	requestSent bool
	cause       error
}

func (e *callFailure) Error() string {
	if e == nil {
		return "<nil>"
	}
	text := string(e.code)
	if e.reported != "" {
		text += ": " + e.reported
	}
	if e.detail != "" {
		text += ": " + e.detail
	}
	return text
}

func (e *callFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return fault.Wrap(e.code, e.detail, contextCause(e.cause))
}

func (e *callFailure) Public() string {
	if e.reported != "" {
		return string(e.code) + ": " + e.reported
	}
	return string(e.code)
}

func failf(code ErrorCode, detail string) *callFailure {
	return &callFailure{code: code, detail: detail}
}

func wrapFailure(code ErrorCode, err error, detail string) *callFailure {
	return &callFailure{code: code, detail: detail, cause: err}
}

func Errorf(code ErrorCode, format string, args ...any) error {
	return fault.New(code, fmt.Sprintf(format, args...))
}

func Wrap(code ErrorCode, err error, detail string) error {
	return fault.Wrap(code, detail, contextCause(err))
}

func authCode(err error) (ErrorCode, bool) {
	code, ok := fault.CodeOf[auth.ErrorCode](err)
	if !ok {
		return "", false
	}
	if known := ErrorCode(code); slices.Contains([]ErrorCode{ErrAuthRequired, ErrAuthFailed, ErrAuthUnavailable}, known) {
		return known, true
	}
	return "", false
}
