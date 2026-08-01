package approval

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	ErrorCodeCapabilityUnavailable ErrorCode = "capability_unavailable"
	ErrorCodePolicyDenied          ErrorCode = "policy_denied"
	ErrorCodeApprovalRequired      ErrorCode = "approval_required"
	ErrorCodeApprovalInvalid       ErrorCode = "approval_invalid"
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
