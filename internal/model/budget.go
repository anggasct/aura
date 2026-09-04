package model

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type budgetContextKey struct{}

func WithInvocationBudget(ctx context.Context, budget *InvocationBudget) context.Context {
	if budget == nil {
		return ctx
	}
	return context.WithValue(ctx, budgetContextKey{}, budget)
}

func BudgetFromContext(ctx context.Context) *InvocationBudget {
	if ctx == nil {
		return nil
	}
	b, _ := ctx.Value(budgetContextKey{}).(*InvocationBudget)
	return b
}

type BudgetParams struct {
	Deadline         time.Time
	MaxAttempts      int
	RetryDelayBudget time.Duration
	MaxOutputTokens  int
	CostCeilingUSD   float64
	Now              func() time.Time
}

type InvocationBudget struct {
	mu sync.Mutex

	now              func() time.Time
	deadline         time.Time
	maxAttempts      int
	retryDelayBudget time.Duration
	maxOutputTokens  int
	costCeilingUSD   float64

	attempts        int
	consumedDelay   time.Duration
	consumedTokens  int
	consumedCostUSD float64
}

func NewInvocationBudget(params BudgetParams) (*InvocationBudget, error) {
	if params.MaxAttempts < 0 {
		return nil, newError(ErrorCodeProtocolInvalid, "", "", "max_attempts must not be negative")
	}
	if params.RetryDelayBudget < 0 {
		return nil, newError(ErrorCodeProtocolInvalid, "", "", "retry_delay_budget must not be negative")
	}
	if params.MaxOutputTokens < 0 {
		return nil, newError(ErrorCodeProtocolInvalid, "", "", "max_output_tokens must not be negative")
	}
	if params.CostCeilingUSD < 0 {
		return nil, newError(ErrorCodeProtocolInvalid, "", "", "cost_ceiling_usd must not be negative")
	}
	nowFn := params.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	return &InvocationBudget{
		now:              nowFn,
		deadline:         params.Deadline,
		maxAttempts:      params.MaxAttempts,
		retryDelayBudget: params.RetryDelayBudget,
		maxOutputTokens:  params.MaxOutputTokens,
		costCeilingUSD:   params.CostCeilingUSD,
	}, nil
}

func (b *InvocationBudget) checkActiveLocked(ctx context.Context) error {
	now := b.now()
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		if d, ok := ctx.Deadline(); ok && now.After(d) {
			return ctx.Err()
		}
	}
	if !b.deadline.IsZero() && now.After(b.deadline) {
		return newError(ErrorCodeDeadlineExceeded, "", "", "invocation deadline exceeded")
	}
	return nil
}

func (b *InvocationBudget) CheckActive(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.checkActiveLocked(ctx)
}

func (b *InvocationBudget) CanAttempt(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.checkActiveLocked(ctx); err != nil {
		return err
	}
	if b.maxAttempts > 0 && b.attempts >= b.maxAttempts {
		return newError(ErrorCodeBudgetExceeded, "", "", fmt.Sprintf("max provider attempts (%d) exceeded", b.maxAttempts))
	}
	return nil
}

func (b *InvocationBudget) RecordAttempt(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.checkActiveLocked(ctx); err != nil {
		return err
	}
	if b.maxAttempts > 0 && b.attempts >= b.maxAttempts {
		return newError(ErrorCodeBudgetExceeded, "", "", fmt.Sprintf("max provider attempts (%d) exceeded", b.maxAttempts))
	}
	b.attempts++
	return nil
}

func (b *InvocationBudget) ReserveDelay(ctx context.Context, delay time.Duration) (time.Duration, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.checkActiveLocked(ctx); err != nil {
		return 0, err
	}
	if delay <= 0 {
		return 0, nil
	}
	now := b.now()
	if ctx != nil {
		if d, ok := ctx.Deadline(); ok && now.Add(delay).After(d) {
			return 0, newError(ErrorCodeDeadlineExceeded, "", "", "retry delay exceeds context deadline")
		}
	}
	if !b.deadline.IsZero() && now.Add(delay).After(b.deadline) {
		return 0, newError(ErrorCodeDeadlineExceeded, "", "", "retry delay exceeds invocation deadline")
	}
	if b.retryDelayBudget > 0 && b.consumedDelay+delay > b.retryDelayBudget {
		return 0, newError(ErrorCodeBudgetExceeded, "", "", fmt.Sprintf("retry delay budget (%v) exceeded", b.retryDelayBudget))
	}
	b.consumedDelay += delay
	return delay, nil
}

func (b *InvocationBudget) RecordTokens(ctx context.Context, tokens int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.checkActiveLocked(ctx); err != nil {
		return err
	}
	if tokens <= 0 {
		return nil
	}
	if b.maxOutputTokens > 0 && b.consumedTokens+tokens > b.maxOutputTokens {
		return newError(ErrorCodeBudgetExceeded, "", "", fmt.Sprintf("output token budget (%d) exceeded", b.maxOutputTokens))
	}
	b.consumedTokens += tokens
	return nil
}

func (b *InvocationBudget) RecordCost(ctx context.Context, costUSD float64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.checkActiveLocked(ctx); err != nil {
		return err
	}
	if costUSD <= 0 {
		return nil
	}
	if b.costCeilingUSD > 0 && b.consumedCostUSD+costUSD > b.costCeilingUSD {
		return newError(ErrorCodeBudgetExceeded, "", "", fmt.Sprintf("cost budget ($%.2f) exceeded", b.costCeilingUSD))
	}
	b.consumedCostUSD += costUSD
	return nil
}

func (b *InvocationBudget) RecordCostMicros(ctx context.Context, costMicros int64) error {
	if costMicros <= 0 {
		return nil
	}
	return b.RecordCost(ctx, float64(costMicros)/1e6)
}

func (b *InvocationBudget) Attempts() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.attempts
}

func (b *InvocationBudget) Consumed() (attempts int, delay time.Duration, tokens int, costUSD float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.attempts, b.consumedDelay, b.consumedTokens, b.consumedCostUSD
}

func (b *InvocationBudget) Remaining(now time.Time) (attempts int, delay time.Duration, tokens int, costUSD float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.maxAttempts > 0 {
		attempts = b.maxAttempts - b.attempts
		if attempts < 0 {
			attempts = 0
		}
	}
	if b.retryDelayBudget > 0 {
		delay = b.retryDelayBudget - b.consumedDelay
		if delay < 0 {
			delay = 0
		}
	}
	if b.maxOutputTokens > 0 {
		tokens = b.maxOutputTokens - b.consumedTokens
		if tokens < 0 {
			tokens = 0
		}
	}
	if b.costCeilingUSD > 0 {
		costUSD = b.costCeilingUSD - b.consumedCostUSD
		if costUSD < 0 {
			costUSD = 0
		}
	}
	return attempts, delay, tokens, costUSD
}

func (b *InvocationBudget) RemainingDeadline(now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.deadline.IsZero() {
		return 0
	}
	if now.IsZero() {
		now = b.now()
	}
	rem := b.deadline.Sub(now)
	if rem < 0 {
		return 0
	}
	return rem
}
