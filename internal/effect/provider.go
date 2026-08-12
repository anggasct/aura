package effect

import (
	"context"
	"encoding/json"
)

// Invocation is the request the Provider executes for one intent. The
// idempotency key is stable across safe retries and is sent when the provider
// supports idempotent re-invocation.
type Invocation struct {
	IntentID       string
	IdempotencyKey string
	Provider       string
	Operation      string
	Classification Classification
	Request        json.RawMessage
}

// Outcome is the result a Provider reports for an invocation.
type Outcome struct {
	// Ambiguous signals the adapter could not confirm whether the provider
	// observed and acted on the request: a timeout, connection loss, process
	// crash, or response-commit failure after invocation began. The intent
	// becomes unknown rather than guessed.
	Ambiguous bool
	// Succeeded is the definite outcome on a non-ambiguous result.
	Succeeded bool
	// Receipt is the provider receipt recorded on success.
	Receipt json.RawMessage
	// SafeErrorCode is a non-secret stable code recorded on a definite failure.
	SafeErrorCode string
}

// Evidence is the result a Reconciler reports when seeking read-only proof of
// an unknown intent's outcome.
type Evidence struct {
	// Definitive is true only when the provider proved a terminal result. When
	// false the intent stays unknown.
	Definitive bool
	Succeeded  bool
	Receipt    json.RawMessage
	// SafeErrorCode is recorded when Definitive and not Succeeded.
	SafeErrorCode string
}

// Provider performs the actual effectful operation for an intent. The
// interface carries no domain terms, so a channel-delivery, webhook,
// broadcaster, or scheduler adapter implements the same contract.
type Provider interface {
	// SupportsIdempotency reports whether the provider accepts the
	// idempotency key for safe re-invocation.
	SupportsIdempotency() bool
	// Invoke performs the operation. An error means the adapter could not
	// classify the result, so the caller treats it as ambiguous.
	Invoke(ctx context.Context, inv *Invocation) (Outcome, error)
}

// Reconciler seeks read-only provider evidence for an unknown intent. A
// provider that supports reconciliation implements this alongside Provider.
type Reconciler interface {
	Reconcile(ctx context.Context, intent *Intent) (Evidence, error)
}

// CanAutoRetry reports whether an unknown intent of this classification may be
// automatically re-invoked. Read-only and idempotent operations are always
// safe; effectful operations only when the provider accepts an idempotency key
// so a re-invoke deduplicates; irreversible operations are never auto-retried.
func CanAutoRetry(c Classification, providerSupportsIdempotency bool) bool {
	switch c {
	case ClassificationReadOnly, ClassificationIdempotent:
		return true
	case ClassificationEffectful:
		return providerSupportsIdempotency
	default:
		return false
	}
}
