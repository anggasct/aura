package usage

import (
	"math"
	"slices"
	"sync"
	"time"
)

type Price struct {
	ModelDefinitionID       string
	Capability              string
	Currency                string
	MicrosPerInputToken     int64
	MicrosPerOutputToken    int64
	MicrosPerCacheToken     int64
	MicrosPerReasoningToken int64
	EffectiveFrom           time.Time
	EffectiveTo             time.Time
	Source                  string
	MaxReservationRate      int64
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

func (p *Price) CostMicros(usage Usage) int64 {
	input := mulChecked(usage.InputTokens, p.MicrosPerInputToken)
	output := mulChecked(usage.OutputTokens, p.MicrosPerOutputToken)
	cache := mulChecked(usage.CacheTokens, p.MicrosPerCacheToken)
	reasoning := mulChecked(usage.ReasoningTokens, p.MicrosPerReasoningToken)
	return input + output + cache + reasoning
}

type Usage struct {
	InputTokens     int64
	OutputTokens    int64
	CacheTokens     int64
	ReasoningTokens int64
}

func (u Usage) valid() bool {
	return u.InputTokens >= 0 && u.OutputTokens >= 0 && u.CacheTokens >= 0 && u.ReasoningTokens >= 0
}

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
