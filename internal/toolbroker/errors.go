package toolbroker

import (
	"errors"
	"fmt"
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
)

type Error struct {
	Class  ResultClass
	Detail string
	Cause  error
}

func (e *Error) Error() string {
	if e.Cause == nil {
		return fmt.Sprintf("%s: %s", e.Class, e.Detail)
	}
	return fmt.Sprintf("%s: %s: %v", e.Class, e.Detail, e.Cause)
}

func (e *Error) Unwrap() error {
	return e.Cause
}

func CodeOf(err error) (ResultClass, bool) {
	var target *Error
	if !errors.As(err, &target) {
		return "", false
	}
	return target.Class, true
}

func errorf(class ResultClass, format string, args ...any) error {
	return &Error{Class: class, Detail: fmt.Sprintf(format, args...)}
}

func Errorf(class ResultClass, format string, args ...any) error {
	return errorf(class, format, args...)
}
