package effect

import (
	"context"
	"errors"
	"fmt"

	"github.com/anggasct/aura/internal/platform/fault"
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
	return e.run(ctx, started, p)
}

type Settler func(ctx context.Context, taken *Intent) (outcome Outcome, settled bool, err error)

func (e *Executor) Resume(ctx context.Context, id string, p Provider, settle Settler) (*Intent, error) {
	if p == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "effect: provider must not be nil", nil)
	}
	taken, err := e.journal.takeOver(ctx, id)
	if err != nil {
		return nil, err
	}
	if settle != nil {
		outcome, settled, err := settle(ctx, taken)
		if err != nil {
			return nil, err
		}
		if settled {
			return e.settleOutcome(ctx, taken, &outcome)
		}
	}
	return e.run(ctx, taken, p)
}

func (e *Executor) run(ctx context.Context, held *Intent, p Provider) (*Intent, error) {
	providerCtx, cancelProvider := context.WithCancel(ctx)
	defer cancelProvider()
	stopLease := e.journal.keepLease(ctx, held, cancelProvider, p)
	defer stopLease()

	outcome, err := p.Invoke(providerCtx, invocationOf(held))
	stopLease()
	if err != nil {
		resolved, markErr := e.journal.MarkUnknown(ctx, held.ID, held.LeaseToken)
		if markErr != nil {
			return nil, fmt.Errorf("effect: mark unknown after invoke error: %w", errors.Join(err, markErr))
		}
		return resolved, err
	}
	return e.settleOutcome(ctx, held, &outcome)
}

func (e *Executor) settleOutcome(ctx context.Context, held *Intent, outcome *Outcome) (*Intent, error) {
	switch {
	case outcome.Ambiguous:
		return e.journal.MarkUnknown(ctx, held.ID, held.LeaseToken)
	case outcome.Succeeded:
		return e.journal.succeedWith(ctx, held, outcome)
	default:
		return e.journal.Fail(ctx, held.ID, held.LeaseToken, outcome.SafeErrorCode)
	}
}

func invocationOf(intent *Intent) *Invocation {
	return &Invocation{
		IntentID:       intent.ID,
		IdempotencyKey: intent.IdempotencyKey,
		Provider:       intent.Provider,
		Operation:      intent.Operation,
		Classification: intent.Classification,
		Request:        intent.RequestJSON,
	}
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
	outcome, err := p.Invoke(ctx, invocationOf(intent))
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
			if fault.HasCode(rerr, ErrorCodeReconciliationUnsupported) {
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
