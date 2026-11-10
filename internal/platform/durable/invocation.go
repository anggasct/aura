package durable

import (
	"context"
	"time"
)

type Invocation interface {
	Run() RunRef
	Payload() []byte
	Signal(ctx context.Context, name string) ([]byte, bool)
	Sleep(d time.Duration) error
	Timer(d time.Duration) <-chan time.Time
	Wait(ctx context.Context, name string, timeout time.Duration) (payload []byte, timedOut bool, ok bool)
	RunAction(ctx context.Context, key string, fn func(ctx context.Context) ([]byte, error)) ([]byte, error)
}
