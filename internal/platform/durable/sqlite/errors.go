package sqlite

import (
	"errors"
	"fmt"

	"github.com/anggasct/aura/internal/platform/durable"
)

type ErrorCode string

const (
	ErrorCodeInvalidArgument      ErrorCode = "invalid_argument"
	ErrorCodeHandlerNotRegistered ErrorCode = "handler_not_registered"
	ErrorCodeCallNotRegistered    ErrorCode = "call_not_registered"
	ErrorCodeRunTerminal          ErrorCode = "run_terminal"
	ErrorCodeApprovalConsumed     ErrorCode = "approval_consumed"
	ErrorCodeActionFailed         ErrorCode = "action_failed"
	ErrorCodeOwnershipLost        ErrorCode = "ownership_lost"
	ErrorCodeHostRunning          ErrorCode = "host_running"
	ErrorCodeStopTimeout          ErrorCode = "stop_timeout"
)

type Error struct {
	Code   ErrorCode
	Detail string
	cause  error
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

func (e *Error) Unwrap() error {
	return e.cause
}

func CodeOf(err error) (ErrorCode, bool) {
	var target *Error
	if !errors.As(err, &target) {
		return "", false
	}
	return target.Code, true
}

func invalidArgument(detail string) error {
	return &Error{Code: ErrorCodeInvalidArgument, Detail: detail}
}

func unknownRun(key string) error {
	return fmt.Errorf("%w: %s", durable.ErrUnknownRun, key)
}

func ownershipLost(key string) error {
	return &Error{Code: ErrorCodeOwnershipLost, Detail: "run " + key + " is owned by another host"}
}
