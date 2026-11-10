package durable

import (
	"context"
	"errors"
	"time"
)

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

type CallRequest struct {
	Service string
	Key     string
	Handler string
	Payload []byte
}

type ResolveApprovalRequest struct {
	ApprovalID string
	Payload    []byte
}

func FormatApprovalAddr(runKey, signal string) string {
	return runKey + "/" + signal
}

func ParseApprovalAddr(id string) (runKey, signal string, err error) {
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == '/' {
			runKey, signal = id[:i], id[i+1:]
			break
		}
	}
	if runKey == "" || signal == "" {
		return "", "", errors.New("durable approval address must be run-key/signal-name")
	}
	return runKey, signal, nil
}

type Runtime interface {
	Start(ctx context.Context, req StartRequest) (RunRef, error)
	Signal(ctx context.Context, run RunRef, name string, payload []byte) error
	Cancel(ctx context.Context, run RunRef) error
	Status(ctx context.Context, run RunRef) (RunStatus, error)
	Call(ctx context.Context, req CallRequest) ([]byte, error)
	ResolveApproval(ctx context.Context, req ResolveApprovalRequest) error
	TurnDeadline(ctx context.Context, sessionID string) (deadline time.Time, ok bool, err error)
}

type Handler func(ctx context.Context, inv Invocation) error

type HandlerRegistrar interface {
	RegisterHandler(name string, fn Handler)
}
