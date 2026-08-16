package effect

import (
	"context"
	"time"
)

type Observation struct {
	IntentID       string
	State          State
	Classification Classification
	Provider       string
	Operation      string
	Outcome        string
	Age            time.Duration
}

type Observer interface {
	Observe(context.Context, *Observation)
}

type observerFunc func(context.Context, Observation)

func (f observerFunc) Observe(ctx context.Context, observation *Observation) {
	f(ctx, *observation)
}

func noopObserver(context.Context, Observation) {}

func (j *Journal) observe(ctx context.Context, intent *Intent, outcome string) {
	if intent == nil {
		return
	}
	age := j.now().UTC().Sub(intent.PreparedAt)
	if age < 0 {
		age = 0
	}
	observation := Observation{
		IntentID:       intent.ID,
		State:          intent.State,
		Classification: intent.Classification,
		Provider:       intent.Provider,
		Operation:      intent.Operation,
		Outcome:        outcome,
		Age:            age,
	}
	j.logger.InfoContext(ctx, "effect state observed",
		"component", "effect",
		"effect_id", observation.IntentID,
		"state", observation.State,
		"classification", observation.Classification,
		"provider", observation.Provider,
		"operation", observation.Operation,
		"outcome", observation.Outcome,
		"age_ms", observation.Age.Milliseconds(),
	)
	j.observer.Observe(ctx, &observation)
}
