package toolbroker

import (
	"context"
	"encoding/json"
	"time"

	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/tools"
)

const recoveryPoll = time.Second

type RecoveryRun struct {
	Provider effect.TakeoverProvider
	Settle   func(ctx context.Context, data json.RawMessage) (effect.Outcome, error)
}

type Recovery func(ctx context.Context, request *ToolRequest, intent *effect.Intent, run *RecoveryRun) error

func checkRecoveryRegistration(definition *tools.Definition, recovery Recovery) error {
	if recovery == nil {
		return nil
	}
	if definition == nil {
		return nilOperationArguments()
	}
	if definition.Operation == "" || !definition.Effectful {
		return errorf(ResultInvalidArgument, "tool %q has a recovery but is not an effectful operation", definition.Name)
	}
	return nil
}

func (b *Broker) recoveryFor(definition *tools.Definition) Recovery {
	if definition == nil || definition.Operation == "" {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.recoveries[definition.Key()]
}

func (b *Broker) recoverIntent(ctx context.Context, call *journaledTool, invocation *effectInvocation, intent *effect.Intent) (*effect.Intent, error) {
	if call == nil || call.recovery == nil || invocation == nil || intent == nil {
		return nil, nilOperationArguments()
	}
	request := call.canonical
	waitCtx, cancel := withCallDeadline(ctx, request.Deadline)
	defer cancel()
	run := &RecoveryRun{Provider: invocation, Settle: invocation.settleFromData}
	current := intent
	for pending(current) {
		err := call.recovery(waitCtx, request, current, run)
		if err == nil {
			return b.effects.Journal().Get(ctx, current.ID)
		}
		held := fault.HasCode(err, effect.ErrorCodeInProgress)
		moved := fault.HasCode(err, effect.ErrorCodeTransitionInvalid)
		if !held && !moved {
			return nil, err
		}
		if held {
			if err := b.awaitRecoveryPoll(waitCtx); err != nil {
				return nil, err
			}
		}
		previous := current.State
		var getErr error
		current, getErr = b.effects.Journal().Get(waitCtx, current.ID)
		if getErr != nil {
			return nil, getErr
		}
		if moved && current.State == previous {
			if err := b.awaitRecoveryPoll(waitCtx); err != nil {
				return nil, err
			}
		}
	}
	return current, nil
}

func pending(intent *effect.Intent) bool {
	return intent.State == effect.StateStarted || intent.State == effect.StateUnknown
}

func (b *Broker) awaitRecoveryPoll(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.clock.Timer(recoveryPoll):
		return nil
	}
}

func (i *effectInvocation) settleFromData(ctx context.Context, data json.RawMessage) (effect.Outcome, error) {
	if i == nil || i.request == nil {
		return effect.Outcome{}, nilOperationArguments()
	}
	result := ToolResult{Class: ResultOK, Output: data}
	return i.complete(ctx, &result)
}
