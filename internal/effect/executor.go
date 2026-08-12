package effect

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// Executor drives the prepare -> start -> invoke -> settle protocol and the
// reconciliation recovery path over a Journal. It owns no state of its own;
// every durable fact lives in the journal.
type Executor struct {
	journal *Journal
	logger  *slog.Logger
}

type ExecutorOptions struct {
	Logger *slog.Logger
}

// NewExecutor builds an executor over journal.
func NewExecutor(journal *Journal, opts ExecutorOptions) (*Executor, error) {
	if journal == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "effect: journal must not be nil", nil)
	}
	if opts.Logger == nil {
		opts.Logger = journal.logger
	}
	return &Executor{journal: journal, logger: opts.Logger}, nil
}

// Journal returns the underlying journal.
func (e *Executor) Journal() *Journal { return e.journal }

// Execute runs the full protocol for one intent: prepare the intent and its
// tool.requested event atomically, transition to started, invoke the provider,
// and settle to succeeded, failed, or unknown.
//
// An intent that already exists (a replay) is returned in its current state
// without re-invoking: Prepare's idempotency deduplicates by
// (provider, operation, idempotency_key) and conflicts on a different digest.
// An intent still in a non-prepared state after Prepare is likewise returned
// as-is, so a caller recovering a started or unknown intent reconciles
// explicitly instead of double-invoking.
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
		// The adapter could not classify the result; if bytes may have been
		// sent the only safe state is unknown.
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

// Reconcile seeks definitive evidence for an unknown intent and resolves it
// only when the evidence is terminal. A nil Reconciler (the provider does not
// support reconciliation) yields effect_reconciliation_unsupported. A
// Reconciler error yields effect_reconciliation_failed. Non-definitive
// evidence leaves the intent unknown.
//
// When the provider cannot reconcile but the classification is safe to
// auto-retry (CanAutoRetry), Reconcile re-invokes the provider with the stable
// idempotency key: a definite result resolves the intent, an ambiguous result
// leaves it unknown. Non-idempotent classifications are never re-invoked.
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

// retryByReinvoke re-invokes the provider with the stable idempotency key and
// resolves the unknown intent on a definite result. An ambiguous result leaves
// the intent unknown; a re-invoke error is surfaced so the caller can retry
// the sweep rather than silently dropping it.
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

// Recover sweeps unknown intents and reconciles each. It is the startup or
// background entry point: for every unknown intent it attempts reconciliation
// (and safe re-invocation where CanAutoRetry allows). Non-idempotent unknown
// intents that lack a reconciler remain unknown for an explicit owner
// decision.
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
		before := unknown[i]
		resolved, rerr := e.Reconcile(ctx, before.ID, p, r)
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

// RecoverySummary counts a recovery sweep's outcomes.
type RecoverySummary struct {
	Scanned      int
	Resolved     int
	StillUnknown int
	Unsupported  int
}
