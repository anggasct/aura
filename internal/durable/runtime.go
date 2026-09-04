package durable

import "context"

// RunState is the observable state of one durable execution.
type RunState string

const (
	RunRunning   RunState = "running"
	RunSuspended RunState = "suspended"
	RunSucceeded RunState = "succeeded"
	RunFailed    RunState = "failed"
	RunCancelled RunState = "cancelled"
)

// RunRef identifies one durable execution; keys are caller-assigned and
// Start is idempotent per key.
type RunRef struct {
	Key string
}

type StartRequest struct {
	// Handler names the registered service handler to run.
	Handler string
	// Key identities the execution; Start is idempotent per key.
	Key     string
	Payload []byte
}

type RunStatus struct {
	State  RunState
	Detail string
}

// Runtime is the durable execution port. Implementations journal handler
// execution so a retry replays completed journal actions without
// re-executing them; the Restate adapter owns that guarantee and the fake
// runtime proves suspension semantics in-process.
type Runtime interface {
	Start(ctx context.Context, req StartRequest) (RunRef, error)
	Signal(ctx context.Context, run RunRef, name string, payload []byte) error
	Cancel(ctx context.Context, run RunRef) error
	Status(ctx context.Context, run RunRef) (RunStatus, error)
}

// Handler is one durable service handler. The context carries invocation
// cancellation; a nil return succeeds the run, an error fails it.
type Handler func(ctx context.Context, inv *Invocation) error

// HandlerRegistrar is implemented by runtimes that accept handler
// registration; the interpreter registers its service when supported.
type HandlerRegistrar interface {
	RegisterHandler(name string, fn Handler)
}
