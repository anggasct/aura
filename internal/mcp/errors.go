package mcp

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	ErrConfigInvalid         ErrorCode = "mcp_config_invalid"
	ErrCapabilityUnavailable ErrorCode = "mcp_capability_unavailable"
	ErrProtocolUnsupported   ErrorCode = "mcp_protocol_unsupported"
	ErrAuthRequired          ErrorCode = "mcp_auth_required"
	ErrEgressDenied          ErrorCode = "mcp_egress_denied"
	ErrMessageTooLarge       ErrorCode = "mcp_message_too_large"
	ErrSchemaInvalid         ErrorCode = "mcp_schema_invalid"
	ErrServerUnavailable     ErrorCode = "mcp_server_unavailable"
	ErrRequestTimeout        ErrorCode = "mcp_request_timeout"
	ErrResultInvalid         ErrorCode = "mcp_result_invalid"
	ErrTrustRequired         ErrorCode = "mcp_trust_required"
)

type Error struct {
	Code   ErrorCode
	Detail string
	Err    error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Detail, e.Err)
	}
	if e.Detail != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Detail)
	}
	return string(e.Code)
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func CodeOf(err error) (ErrorCode, bool) {
	var mcpErr *Error
	if errors.As(err, &mcpErr) && mcpErr != nil {
		return mcpErr.Code, true
	}
	return "", false
}

func Errorf(code ErrorCode, format string, args ...any) *Error {
	return &Error{
		Code:   code,
		Detail: fmt.Sprintf(format, args...),
	}
}

func Wrap(code ErrorCode, err error, detail string) *Error {
	return &Error{
		Code:   code,
		Detail: detail,
		Err:    err,
	}
}
