package operation

import (
	"context"
	"encoding/json"

	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

func (r *Registry) recovery(found *entry) toolbroker.Recovery {
	return func(ctx context.Context, request *toolbroker.ToolRequest, intent *effect.Intent, run *toolbroker.RecoveryRun) error {
		if request == nil {
			return nilArgument("tool request")
		}
		call := agentCall(request)
		call.recovery = run
		_, err := r.resume(ctx, found, intent, call)
		return err
	}
}

func (r *Registry) Resume(ctx context.Context, intent *effect.Intent, call *Call) (Result, error) {
	if intent == nil || call == nil {
		return Result{}, nilArgument("intent or call")
	}
	found, ok := r.byTool[intent.Operation]
	if !ok {
		found, ok = r.entries[intent.Operation]
	}
	if !ok {
		return Result{}, newError(ErrorCodeUnknown, "operation: no such operation")
	}
	return r.resume(ctx, found, intent, call)
}

func (r *Registry) resume(ctx context.Context, found *entry, intent *effect.Intent, call *Call) (Result, error) {
	if call.recovery == nil || call.recovery.Provider == nil || call.recovery.Settle == nil {
		return Result{}, newError(ErrorCodeInvalidArgument, "operation: a resumed call needs the caller's recovery hooks")
	}
	switch intent.State {
	case effect.StateStarted:
		return r.resumeStarted(ctx, found, intent, call)
	case effect.StateUnknown:
		return r.resolveUnknown(ctx, found, intent, call)
	case effect.StatePrepared, effect.StateSucceeded, effect.StateFailed:
		return Result{}, fault.New(effect.ErrorCodeTransitionInvalid, "operation: only started and unknown intents resume")
	}
	return Result{}, fault.New(effect.ErrorCodeTransitionInvalid, "operation: only started and unknown intents resume")
}

func (r *Registry) expired(intent *effect.Intent) bool {
	return intent.PreparedAt.Before(r.clock.Now().Add(-ReceiptRetention))
}

func (r *Registry) settler(found *entry, call *Call) effect.Settler {
	return func(ctx context.Context, taken *effect.Intent) (effect.Outcome, bool, error) {
		switch found.descriptor.Commit {
		case CommitTransactional:
			return r.settleFromReceipt(ctx, taken, call)
		case CommitExternalIdempotent:
			return effect.Outcome{}, false, nil
		case CommitExternal, CommitNone:
			return effect.Outcome{Ambiguous: true}, true, nil
		}
		return effect.Outcome{Ambiguous: true}, true, nil
	}
}

func (r *Registry) settleFromReceipt(ctx context.Context, taken *effect.Intent, call *Call) (effect.Outcome, bool, error) {
	stored, present, err := r.receipts.lookup(ctx, taken.IdempotencyKey)
	if err != nil {
		return effect.Outcome{}, false, err
	}
	if present {
		recorded, err := recordedResult(&stored, call.RequestDigest)
		if err != nil {
			return effect.Outcome{}, false, err
		}
		outcome, err := call.recovery.Settle(ctx, recorded.Data)
		if err != nil {
			return effect.Outcome{}, false, err
		}
		return outcome, true, nil
	}
	if r.expired(taken) {
		return effect.Outcome{Ambiguous: true}, true, nil
	}
	return effect.Outcome{}, false, nil
}

func (r *Registry) resumeStarted(ctx context.Context, found *entry, intent *effect.Intent, call *Call) (Result, error) {
	settled, err := r.effects.Resume(ctx, intent.ID, call.recovery.Provider, r.settler(found, call))
	if err != nil {
		return Result{}, err
	}
	return r.settledResult(ctx, settled)
}

func (r *Registry) settledResult(ctx context.Context, settled *effect.Intent) (Result, error) {
	switch settled.State {
	case effect.StateSucceeded:
		return r.recordedData(ctx, settled)
	case effect.StateFailed:
		return Result{}, failedIntentError(settled)
	case effect.StatePrepared, effect.StateStarted, effect.StateUnknown:
		return Result{}, fault.New(effect.ErrorCodeUnknown, "operation: the call is left unknown for the owner")
	}
	return Result{}, fault.New(effect.ErrorCodeUnknown, "operation: the call is left unknown for the owner")
}

func failedIntentError(settled *effect.Intent) error {
	if settled.SafeErrorCode == string(ErrorCodeNotCommitted) {
		return newError(ErrorCodeNotCommitted, "operation: the call did not commit")
	}
	return newError(ErrorCodeFailed, "operation: the call failed")
}

func (r *Registry) recordedData(ctx context.Context, settled *effect.Intent) (Result, error) {
	record, err := r.effects.Journal().RecordedResult(ctx, settled)
	if err != nil {
		return Result{}, err
	}
	var decoded toolbroker.ToolResult
	if err := json.Unmarshal(record, &decoded); err != nil {
		return Result{}, wrapError(ErrorCodeFailed, "a recorded result is unreadable", err)
	}
	return Result{Data: decoded.Output}, nil
}

func (r *Registry) resolveUnknown(ctx context.Context, found *entry, intent *effect.Intent, call *Call) (Result, error) {
	if found.descriptor.Commit != CommitTransactional {
		return Result{}, fault.New(effect.ErrorCodeUnknown, "operation: the call is left unknown for the owner")
	}
	stored, present, err := r.receipts.lookup(ctx, intent.IdempotencyKey)
	if err != nil {
		return Result{}, err
	}
	var resolution effect.Resolution
	switch {
	case present:
		recorded, err := recordedResult(&stored, call.RequestDigest)
		if err != nil {
			return Result{}, err
		}
		outcome, err := call.recovery.Settle(ctx, recorded.Data)
		if err != nil {
			return Result{}, err
		}
		resolution = effect.Resolution{Succeeded: true, Receipt: outcome.Receipt, Result: outcome.Result}
	case r.expired(intent):
		return Result{}, fault.New(effect.ErrorCodeUnknown, "operation: the call is too old to resolve and is left for the owner")
	default:
		resolution = effect.Resolution{SafeErrorCode: string(ErrorCodeNotCommitted)}
	}
	resolved, err := r.effects.Journal().Resolve(ctx, intent.ID, resolution)
	if err != nil {
		if fault.HasCode(err, effect.ErrorCodeTransitionInvalid) {
			return r.afterLostResolution(ctx, intent.ID)
		}
		return Result{}, err
	}
	return r.settledResult(ctx, resolved)
}

func (r *Registry) afterLostResolution(ctx context.Context, id string) (Result, error) {
	current, err := r.effects.Journal().Get(ctx, id)
	if err != nil {
		return Result{}, err
	}
	if current.State == effect.StateUnknown || current.State == effect.StateStarted {
		return Result{}, newError(ErrorCodeFailed, "operation: the intent changed state while it was resolved")
	}
	return r.settledResult(ctx, current)
}
