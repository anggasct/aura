package toolbroker

import (
	"fmt"

	"github.com/anggasct/aura/internal/platform/fault"
)

type ResultClass string

const (
	ResultOK                    ResultClass = "ok"
	ResultInvalidArgument       ResultClass = "invalid_argument"
	ResultPolicyDenied          ResultClass = "policy_denied"
	ResultApprovalRequired      ResultClass = "approval_required"
	ResultCapabilityUnavailable ResultClass = "capability_unavailable"
	ResultDeadlineExceeded      ResultClass = "deadline_exceeded"
	ResultExecutionFailed       ResultClass = "execution_failed"
	ResultToolTierDenied        ResultClass = "tool_tier_denied"

	ResultApprovalDisplayTooLarge  ResultClass = "approval_display_too_large"
	ResultUntrustedInputNoApprover ResultClass = "untrusted_input_no_approver"
)

func (c ResultClass) stable() bool {
	switch c {
	case ResultOK, ResultInvalidArgument, ResultPolicyDenied, ResultApprovalRequired,
		ResultCapabilityUnavailable, ResultDeadlineExceeded, ResultExecutionFailed, ResultToolTierDenied,
		ResultApprovalDisplayTooLarge, ResultUntrustedInputNoApprover:
		return true
	}
	return false
}

func errorf(class ResultClass, format string, args ...any) error {
	return fault.New(class, fmt.Sprintf(format, args...))
}

func Errorf(class ResultClass, format string, args ...any) error {
	return errorf(class, format, args...)
}

type NotPerformedError struct {
	Code string
	Err  error
}

func (e *NotPerformedError) Error() string {
	if e.Err == nil {
		return e.Code
	}
	return e.Err.Error()
}

func (e *NotPerformedError) Unwrap() error {
	return e.Err
}

func NotPerformed(code string, err error) error {
	return &NotPerformedError{Code: code, Err: err}
}
