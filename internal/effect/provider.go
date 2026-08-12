package effect

import (
	"context"
	"encoding/json"
)

type Invocation struct {
	IntentID       string
	IdempotencyKey string
	Provider       string
	Operation      string
	Classification Classification
	Request        json.RawMessage
}

type Outcome struct {
	// Ambiguous signals the adapter could not confirm whether the provider
	// observed and acted on the request; the intent becomes unknown.
	Ambiguous bool
	Succeeded bool
	Receipt   json.RawMessage
	// SafeErrorCode is a non-secret stable code recorded on a definite failure.
	SafeErrorCode string
}

type Evidence struct {
	// Definitive is true only when the provider proved a terminal result;
	// otherwise the intent stays unknown.
	Definitive    bool
	Succeeded     bool
	Receipt       json.RawMessage
	SafeErrorCode string
}

// Provider and Reconciler carry no domain terms so channel, webhook,
// broadcaster, and scheduler adapters implement the same contract.
type Provider interface {
	SupportsIdempotency() bool
	// Invoke's error means the adapter could not classify the result; the
	// caller treats it as ambiguous.
	Invoke(ctx context.Context, inv *Invocation) (Outcome, error)
}

type Reconciler interface {
	Reconcile(ctx context.Context, intent *Intent) (Evidence, error)
}

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
