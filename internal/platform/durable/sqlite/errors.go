package sqlite

import (
	"fmt"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/fault"
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

func invalidArgument(detail string) error {
	return fault.New(ErrorCodeInvalidArgument, detail)
}

func unknownRun(key string) error {
	return fmt.Errorf("%w: %s", durable.ErrUnknownRun, key)
}

func handlerNotRegistered(name string) error {
	return fault.New(ErrorCodeHandlerNotRegistered, "no durable handler registered or declared for "+name)
}

func keyInUse(key string) error {
	return fault.Wrap(ErrorCodeKeyInUse, "durable run "+key+" belongs to another handler", durable.ErrKeyInUse)
}

func hostLease(cause error) error {
	return fault.Wrap(ErrorCodeHostLease, "the durable host could not write its lease", cause)
}

func ownershipLost(key string) error {
	return fault.New(ErrorCodeOwnershipLost, "run "+key+" is owned by another host")
}
