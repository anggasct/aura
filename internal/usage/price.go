package usage

import (
	"math"
	"slices"
	"sync"
	"time"
)

// Price is one versioned cost record for a model definition. Money is integer
// USD micros per token; floating-point money is forbidden. Rates apply as
// micros per token so a reservation can be computed from integer arithmetic.
type Price struct {
	// ModelDefinitionID names the config model definition this price applies
	// to. It is never a free-form runtime model name.
	ModelDefinitionID string
	// Capability is the routing capability (agent, summarize, ...); empty
	// means the price applies to the model definition regardless of role.
	Capability string
	// Currency is the ISO 4217 code; ledger budgets compare in one currency.
	Currency string
	// MicrosPerInputToken, MicrosPerOutputToken, MicrosPerCacheToken, and
	// MicrosPerReasoningToken are the per-token costs in integer USD micros.
	MicrosPerInputToken     int64
	MicrosPerOutputToken    int64
	MicrosPerCacheToken     int64
	MicrosPerReasoningToken int64
	// EffectiveFrom and EffectiveTo bound the interval during which this
	// version applies; EffectiveTo zero means open-ended.
	EffectiveFrom time.Time
	EffectiveTo   time.Time
	// Source records where the price came from (operator file, provider
	// sheet) for auditability.
	Source string
	// MaxReservationRate is the conservative multiplier (in percent, 100 =
	// 1x) applied on top of the estimate when reserving, so unknown usage
	// never settles at zero.
	MaxReservationRate int64
}

func (p *Price) valid() bool {
	return p.ModelDefinitionID != "" &&
		p.Currency != "" &&
		p.MicrosPerInputToken >= 0 &&
		p.MicrosPerOutputToken >= 0 &&
		p.MicrosPerCacheToken >= 0 &&
		p.MicrosPerReasoningToken >= 0 &&
		p.MaxReservationRate >= 100
}

func (p *Price) appliesAt(modelDefinitionID, currency string, at time.Time) bool {
	if p.ModelDefinitionID != modelDefinitionID || p.Currency != currency {
		return false
	}
	if at.Before(p.EffectiveFrom) {
		return false
	}
	return p.EffectiveTo.IsZero() || !at.After(p.EffectiveTo)
}

// ReserveCostMicros computes the conservative reservation for a known input
// token count and a requested maximum output, applying the reservation rate.
// The result is at least 1 micro so an unknown cost is never recorded as zero.
func (p *Price) ReserveCostMicros(inputTokens, requestedMaxOutputTokens int64) int64 {
	in := mulChecked(inputTokens, p.MicrosPerInputToken)
	out := mulChecked(requestedMaxOutputTokens, p.MicrosPerOutputToken)
	estimate := in + out
	reserved := estimate * p.MaxReservationRate / 100
	if reserved < 1 {
		return 1
	}
	return reserved
}

// CostMicros computes the settled cost for reported usage. A zero total with
// positive usage means the price record carries zero rates; the caller's
// conservative policy decides whether that is acceptable.
func (p *Price) CostMicros(usage Usage) int64 {
	input := mulChecked(usage.InputTokens, p.MicrosPerInputToken)
	output := mulChecked(usage.OutputTokens, p.MicrosPerOutputToken)
	cache := mulChecked(usage.CacheTokens, p.MicrosPerCacheToken)
	reasoning := mulChecked(usage.ReasoningTokens, p.MicrosPerReasoningToken)
	return input + output + cache + reasoning
}

// Usage holds provider-reported token counts. All fields are non-negative
// integers; cache and reasoning are zero when the provider does not report
// them.
type Usage struct {
	InputTokens     int64
	OutputTokens    int64
	CacheTokens     int64
	ReasoningTokens int64
}

func (u Usage) valid() bool {
	return u.InputTokens >= 0 && u.OutputTokens >= 0 && u.CacheTokens >= 0 && u.ReasoningTokens >= 0
}

// PriceRegistry is a thread-safe set of versioned price records. Lookups use
// the effective interval so a reservation at a point in time always sees the
// version that was current then.
type PriceRegistry struct {
	mu     sync.RWMutex
	prices []Price
}

func NewPriceRegistry() *PriceRegistry {
	return &PriceRegistry{}
}

func (r *PriceRegistry) Put(p *Price) error {
	if !p.valid() {
		return codedError(ErrorCodePriceVersionInvalid, "invalid price record", nil)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prices = append(r.prices, *p)
	return nil
}

// At returns the price for a model definition and currency effective at the
// given time. When multiple records overlap, the most recently effective one
// wins; a nil result means no applicable record.
func (r *PriceRegistry) At(modelDefinitionID, currency string, at time.Time) (*Price, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var best *Price
	for i := range r.prices {
		p := &r.prices[i]
		if !p.appliesAt(modelDefinitionID, currency, at) {
			continue
		}
		if best == nil || p.EffectiveFrom.After(best.EffectiveFrom) {
			best = p
		}
	}
	return best, nil
}

// All returns a snapshot of every record, sorted by model definition then
// effective start, for stable reporting.
func (r *PriceRegistry) All() []Price {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := slices.Clone(r.prices)
	slices.SortFunc(out, func(a, b Price) int {
		if a.ModelDefinitionID != b.ModelDefinitionID {
			if a.ModelDefinitionID < b.ModelDefinitionID {
				return -1
			}
			return 1
		}
		return a.EffectiveFrom.Compare(b.EffectiveFrom)
	})
	return out
}

func mulChecked(a, b int64) int64 {
	if a == 0 || b == 0 {
		return 0
	}
	if a > math.MaxInt64/b {
		return math.MaxInt64
	}
	return a * b
}
