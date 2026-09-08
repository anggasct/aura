package restate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	restate "github.com/restatedev/sdk-go"

	"github.com/anggasct/aura/internal/durable"
)

type invocation struct {
	runtime restate.WorkflowContext
	ref     durable.RunRef
	payload []byte
}

func (i *invocation) Run() durable.RunRef { return i.ref }

func (i *invocation) Payload() []byte { return i.payload }

func (i *invocation) Signal(ctx context.Context, name string) ([]byte, bool) {
	if name == "" {
		return nil, false
	}
	promise := restate.Promise[json.RawMessage](i.runtime, name)
	output, err := promise.Result()
	if err != nil {
		return nil, false
	}
	return []byte(output), true
}

func (i *invocation) Sleep(d time.Duration) error {
	if err := restate.Sleep(i.runtime, d); err != nil {
		return errors.New(err.Error())
	}
	return nil
}

func (i *invocation) Timer(time.Duration) <-chan time.Time {
	panic("restate invocation does not expose timers as channels; await signals with Wait")
}

func (i *invocation) Wait(ctx context.Context, name string, timeout time.Duration) (payload []byte, timedOut, ok bool) {
	if name == "" {
		return nil, false, false
	}
	promise := restate.Promise[json.RawMessage](i.runtime, name)
	timer := restate.After(i.runtime, timeout)
	if _, err := restate.WaitFirst(i.runtime, promise, timer); err != nil {
		return nil, false, false
	}
	output, err := promise.Peek()
	if err != nil {
		return nil, false, false
	}
	if output == nil {
		return nil, true, true
	}
	return []byte(output), false, true
}

func (i *invocation) RunAction(ctx context.Context, key string, fn func(ctx context.Context) ([]byte, error)) ([]byte, error) {
	if key == "" {
		return nil, errors.New("restate run action requires a key")
	}
	if fn == nil {
		return nil, errors.New("restate run action requires a function")
	}
	output, err := restate.Run(i.runtime, func(_ restate.RunContext) ([]byte, error) {
		return fn(ctx)
	}, restate.WithName(key), restate.WithMaxRetryAttempts(1))
	if err != nil {
		return nil, fmt.Errorf("restate run action %q: %w", key, err)
	}
	return output, nil
}
