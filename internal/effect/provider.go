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
	Ambiguous     bool
	Succeeded     bool
	Receipt       json.RawMessage
	SafeErrorCode string
}

type Evidence struct {
	Definitive    bool
	Succeeded     bool
	Receipt       json.RawMessage
	SafeErrorCode string
}

type Provider interface {
	SupportsIdempotency() bool
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
