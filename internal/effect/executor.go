package effect

import (
	"context"
	"errors"
	"fmt"
)

type Executor struct {
	journal *Journal
}

func NewExecutor(journal *Journal) (*Executor, error) {
	if journal == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "effect: journal must not be nil", nil)
	}
	return &Executor{journal: journal}, nil
}

func (e *Executor) Journal() *Journal { return e.journal }

func (e *Executor) Execute(ctx context.Context, req *PrepareRequest, p Provider) (*Intent, error) {
	if p == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "effect: provider must not be nil", nil)
	}
	intent, err := e.journal.Prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	if intent.State != StatePrepared {
		return intent, nil
	}

	started, err := e.journal.Start(ctx, intent.ID)
	if err != nil {
		return nil, err
	}

	outcome, err := p.Invoke(ctx, &Invocation{
		IntentID:       started.ID,
		IdempotencyKey: started.IdempotencyKey,
		Provider:       started.Provider,
		Operation:      started.Operation,
		Classification: started.Classification,
		Request:        started.RequestJSON,
	})
	if err != nil {
		resolved, markErr := e.journal.MarkUnknown(ctx, started.ID)
		if markErr != nil {
			return nil, fmt.Errorf("effect: mark unknown after invoke error: %w", errors.Join(err, markErr))
		}
		return resolved, err
	}

	if outcome.Ambiguous {
		return e.journal.MarkUnknown(ctx, started.ID)
	}
	if outcome.Succeeded {
		return e.journal.Succeed(ctx, started.ID, outcome.Receipt)
	}
	return e.journal.Fail(ctx, started.ID, outcome.SafeErrorCode)
}

func (e *Executor) Reconcile(ctx context.Context, id string, p Provider, r Reconciler) (*Intent, error) {
	if p == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "effect: provider must not be nil", nil)
	}
	intent, err := e.journal.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if intent.State != StateUnknown {
		return nil, codedError(ErrorCodeTransitionInvalid,
			fmt.Sprintf("effect: intent %s is %s, only unknown intents reconcile", id, intent.State), nil)
	}

	if r != nil {
		evidence, err := r.Reconcile(ctx, intent)
		if err != nil {
			return nil, codedError(ErrorCodeReconciliationFailed,
				fmt.Sprintf("effect: reconcile intent %s failed", id), err)
		}
		if evidence.Definitive {
			return e.journal.Resolve(ctx, id, Resolution{
				Succeeded:     evidence.Succeeded,
				Receipt:       evidence.Receipt,
				SafeErrorCode: evidence.SafeErrorCode,
			})
		}
	}

	if CanAutoRetry(intent.Classification, p.SupportsIdempotency()) {
		return e.retryByReinvoke(ctx, intent, p)
	}

	if r == nil {
		return nil, codedError(ErrorCodeReconciliationUnsupported,
			fmt.Sprintf("effect: provider %s does not support reconciliation for intent %s", intent.Provider, id), nil)
	}
	return intent, nil
}

func (e *Executor) retryByReinvoke(ctx context.Context, intent *Intent, p Provider) (*Intent, error) {
	outcome, err := p.Invoke(ctx, &Invocation{
		IntentID:       intent.ID,
		IdempotencyKey: intent.IdempotencyKey,
		Provider:       intent.Provider,
		Operation:      intent.Operation,
		Classification: intent.Classification,
		Request:        intent.RequestJSON,
	})
	if err != nil {
		return intent, err
	}
	if outcome.Ambiguous {
		return intent, nil
	}
	if outcome.Succeeded {
		return e.journal.Resolve(ctx, intent.ID, Resolution{
			Succeeded: true,
			Receipt:   outcome.Receipt,
		})
	}
	return e.journal.Resolve(ctx, intent.ID, Resolution{
		Succeeded:     false,
		SafeErrorCode: outcome.SafeErrorCode,
	})
}

func (e *Executor) Recover(ctx context.Context, p Provider, r Reconciler) (RecoverySummary, error) {
	if p == nil {
		return RecoverySummary{}, codedError(ErrorCodeInvalidArgument, "effect: provider must not be nil", nil)
	}
	unknown, err := e.journal.ListByState(ctx, StateUnknown, 0)
	if err != nil {
		return RecoverySummary{}, err
	}
	summary := RecoverySummary{Scanned: len(unknown)}
	for i := range unknown {
		resolved, rerr := e.Reconcile(ctx, unknown[i].ID, p, r)
		if rerr != nil {
			if code, ok := CodeOf(rerr); ok && code == ErrorCodeReconciliationUnsupported {
				summary.Unsupported++
				continue
			}
			return summary, rerr
		}
		if resolved.State == StateSucceeded || resolved.State == StateFailed {
			summary.Resolved++
		} else {
			summary.StillUnknown++
		}
	}
	return summary, nil
}

type RecoverySummary struct {
	Scanned      int
	Resolved     int
	StillUnknown int
	Unsupported  int
}
