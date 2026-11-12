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
	ErrorCodeHostLease            ErrorCode = "host_lease"
	ErrorCodeStopTimeout          ErrorCode = "stop_timeout"
	ErrorCodeSessionStateInvalid  ErrorCode = "session_state_invalid"
	ErrorCodeKeyInUse             ErrorCode = "key_in_use"
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

func handlerNotRegistered(name string) error {
	return &Error{Code: ErrorCodeHandlerNotRegistered, Detail: "no durable handler registered or declared for " + name}
}

func keyInUse(key string) error {
	return &Error{Code: ErrorCodeKeyInUse, Detail: "durable run " + key + " belongs to another handler", cause: durable.ErrKeyInUse}
}

func hostLease(cause error) error {
	return &Error{Code: ErrorCodeHostLease, Detail: "the durable host could not write its lease: " + cause.Error(), cause: cause}
}

func ownershipLost(key string) error {
	return &Error{Code: ErrorCodeOwnershipLost, Detail: "run " + key + " is owned by another host"}
}
