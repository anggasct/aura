package approval

import (
	"fmt"

	"github.com/anggasct/aura/internal/platform/fault"
)

type ErrorCode string

const (
	ErrorCodeInvalidArgument       ErrorCode = "invalid_argument"
	ErrorCodeCapabilityUnavailable ErrorCode = "capability_unavailable"
	ErrorCodePolicyDenied          ErrorCode = "policy_denied"
	ErrorCodeApprovalRequired      ErrorCode = "approval_required"
	ErrorCodeApprovalInvalid       ErrorCode = "approval_invalid"
)

func Errorf(code ErrorCode, format string, args ...any) error {
	return fault.New(code, fmt.Sprintf(format, args...))
}
