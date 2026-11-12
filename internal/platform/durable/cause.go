package durable

import (
	"context"
	"errors"
)

var (
	ErrRunCancelled  = errors.New("durable run cancelled")
	ErrHostStopping  = errors.New("durable host stopping")
	ErrOwnershipLost = errors.New("durable run ownership lost")
)

func Preempted(ctx context.Context) bool {
	if ctx.Err() == nil {
		return false
	}
	cause := context.Cause(ctx)
	return errors.Is(cause, ErrHostStopping) || errors.Is(cause, ErrOwnershipLost)
}
