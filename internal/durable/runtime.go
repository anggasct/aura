package durable

import "context"

type RunState string

const (
	RunRunning   RunState = "running"
	RunSuspended RunState = "suspended"
	RunSucceeded RunState = "succeeded"
	RunFailed    RunState = "failed"
	RunCancelled RunState = "cancelled"
)

type RunRef struct {
	Key string
}

type StartRequest struct {
	Handler string
	Key     string
	Payload []byte
}

type RunStatus struct {
	State  RunState
	Detail string
}

type Runtime interface {
	Start(ctx context.Context, req StartRequest) (RunRef, error)
	Signal(ctx context.Context, run RunRef, name string, payload []byte) error
	Cancel(ctx context.Context, run RunRef) error
	Status(ctx context.Context, run RunRef) (RunStatus, error)
}

type Handler func(ctx context.Context, inv Invocation) error

type HandlerRegistrar interface {
	RegisterHandler(name string, fn Handler)
}
