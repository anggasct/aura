package mcp

import (
	"fmt"

	"github.com/anggasct/aura/internal/platform/fault"
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

func Errorf(code ErrorCode, format string, args ...any) error {
	return fault.New(code, fmt.Sprintf(format, args...))
}

func Wrap(code ErrorCode, err error, detail string) error {
	return fault.Wrap(code, detail, err)
}
