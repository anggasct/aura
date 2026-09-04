package model

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestNewInvocationBudget_Validation(t *testing.T) {
	tests := []struct {
		name    string
		params  BudgetParams
		wantErr bool
	}{
		{
			name:    "valid-unbounded",
			params:  BudgetParams{},
			wantErr: false,
		},
		{
			name: "valid-bounded",
			params: BudgetParams{
				Deadline:         time.Now().Add(time.Minute),
				MaxAttempts:      4,
				RetryDelayBudget: 20 * time.Second,
				MaxOutputTokens:  4096,
				CostCeilingUSD:   1.50,
			},
			wantErr: false,
		},
		{
			name:    "negative-max-attempts",
			params:  BudgetParams{MaxAttempts: -1},
			wantErr: true,
		},
		{
			name:    "negative-retry-delay",
			params:  BudgetParams{RetryDelayBudget: -time.Second},
			wantErr: true,
		},
		{
			name:    "negative-tokens",
			params:  BudgetParams{MaxOutputTokens: -10},
			wantErr: true,
		},
		{
			name:    "negative-cost",
			params:  BudgetParams{CostCeilingUSD: -0.01},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := NewInvocationBudget(tt.params)
			if tt.wantErr {
				wantCode(t, err, ErrorCodeProtocolInvalid)
				if b != nil {
					t.Fatalf("expected nil budget on error")
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if b == nil {
					t.Fatalf("expected non-nil budget")
				}
			}
		})
	}
}

func TestInvocationBudget_DeadlineExceeded(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	currTime := now
	nowFn := func() time.Time { return currTime }

	deadline := now.Add(10 * time.Second)
	b, err := NewInvocationBudget(BudgetParams{
		Deadline: deadline,
		Now:      nowFn,
	})
	if err != nil {
		t.Fatalf("NewInvocationBudget: %v", err)
	}

	ctx := context.Background()

	// Initially active
	if err := b.CheckActive(ctx); err != nil {
		t.Fatalf("expected active budget, got %v", err)
	}

	// Advance time past deadline
	currTime = now.Add(11 * time.Second)
	err = b.CheckActive(ctx)
	wantCode(t, err, ErrorCodeDeadlineExceeded)

	err = b.RecordAttempt(ctx)
	wantCode(t, err, ErrorCodeDeadlineExceeded)

	err = b.CanAttempt(ctx)
	wantCode(t, err, ErrorCodeDeadlineExceeded)
}

func TestInvocationBudget_ContextCancellationStopsWork(t *testing.T) {
	b, err := NewInvocationBudget(BudgetParams{MaxAttempts: 4})
	if err != nil {
		t.Fatalf("NewInvocationBudget: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	if err := b.CheckActive(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("CheckActive: err = %v, want context.Canceled", err)
	}

	if err := b.CanAttempt(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("CanAttempt: err = %v, want context.Canceled", err)
	}

	if err := b.RecordAttempt(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("RecordAttempt: err = %v, want context.Canceled", err)
	}

	if _, err := b.ReserveDelay(ctx, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("ReserveDelay: err = %v, want context.Canceled", err)
	}

	if err := b.RecordTokens(ctx, 100); !errors.Is(err, context.Canceled) {
		t.Fatalf("RecordTokens: err = %v, want context.Canceled", err)
	}

	if err := b.RecordCost(ctx, 0.5); !errors.Is(err, context.Canceled) {
		t.Fatalf("RecordCost: err = %v, want context.Canceled", err)
	}
}

func TestInvocationBudget_AttemptsCeiling(t *testing.T) {
	b, err := NewInvocationBudget(BudgetParams{MaxAttempts: 3})
	if err != nil {
		t.Fatalf("NewInvocationBudget: %v", err)
	}
	ctx := context.Background()

	for i := 1; i <= 3; i++ {
		if err := b.CanAttempt(ctx); err != nil {
			t.Fatalf("attempt %d CanAttempt failed: %v", i, err)
		}
		if err := b.RecordAttempt(ctx); err != nil {
			t.Fatalf("attempt %d RecordAttempt failed: %v", i, err)
		}
		if b.Attempts() != i {
			t.Fatalf("Attempts() = %d, want %d", b.Attempts(), i)
		}
	}

	// 4th attempt must be rejected
	err = b.CanAttempt(ctx)
	wantCode(t, err, ErrorCodeBudgetExceeded)

	err = b.RecordAttempt(ctx)
	wantCode(t, err, ErrorCodeBudgetExceeded)
}

func TestInvocationBudget_RetryDelayBudget(t *testing.T) {
	b, err := NewInvocationBudget(BudgetParams{RetryDelayBudget: 15 * time.Second})
	if err != nil {
		t.Fatalf("NewInvocationBudget: %v", err)
	}
	ctx := context.Background()

	reserved, err := b.ReserveDelay(ctx, 5*time.Second)
	if err != nil || reserved != 5*time.Second {
		t.Fatalf("ReserveDelay(5s) = %v, %v; want 5s, nil", reserved, err)
	}

	reserved, err = b.ReserveDelay(ctx, 8*time.Second)
	if err != nil || reserved != 8*time.Second {
		t.Fatalf("ReserveDelay(8s) = %v, %v; want 8s, nil", reserved, err)
	}

	// Consumed 13s, budget is 15s. Next 3s reservation exceeds 15s.
	_, err = b.ReserveDelay(ctx, 3*time.Second)
	wantCode(t, err, ErrorCodeBudgetExceeded)

	// Zero or negative delay is allowed and consumes nothing
	reserved, err = b.ReserveDelay(ctx, 0)
	if err != nil || reserved != 0 {
		t.Fatalf("ReserveDelay(0) = %v, %v; want 0, nil", reserved, err)
	}
}

func TestInvocationBudget_DelayExceedsDeadlines(t *testing.T) {
	now := time.Now()
	currTime := now
	nowFn := func() time.Time { return currTime }

	invocationDeadline := now.Add(10 * time.Second)
	b, err := NewInvocationBudget(BudgetParams{
		Deadline:         invocationDeadline,
		RetryDelayBudget: 30 * time.Second,
		Now:              nowFn,
	})
	if err != nil {
		t.Fatalf("NewInvocationBudget: %v", err)
	}

	ctx := context.Background()

	// Delay of 12s exceeds invocation deadline (10s)
	_, err = b.ReserveDelay(ctx, 12*time.Second)
	wantCode(t, err, ErrorCodeDeadlineExceeded)

	// Context with shorter deadline
	ctxShort, cancel := context.WithDeadline(context.Background(), now.Add(5*time.Second))
	defer cancel()

	// Delay of 6s exceeds context deadline (5s)
	_, err = b.ReserveDelay(ctxShort, 6*time.Second)
	wantCode(t, err, ErrorCodeDeadlineExceeded)
}

func TestInvocationBudget_TokenCeiling(t *testing.T) {
	b, err := NewInvocationBudget(BudgetParams{MaxOutputTokens: 1000})
	if err != nil {
		t.Fatalf("NewInvocationBudget: %v", err)
	}
	ctx := context.Background()

	if err := b.RecordTokens(ctx, 600); err != nil {
		t.Fatalf("RecordTokens(600) failed: %v", err)
	}

	// 500 more exceeds 1000 limit
	err = b.RecordTokens(ctx, 500)
	wantCode(t, err, ErrorCodeBudgetExceeded)

	// 0 or negative tokens allowed
	if err := b.RecordTokens(ctx, -5); err != nil {
		t.Fatalf("RecordTokens(-5) failed: %v", err)
	}
}

func TestInvocationBudget_CostCeiling(t *testing.T) {
	b, err := NewInvocationBudget(BudgetParams{CostCeilingUSD: 2.00})
	if err != nil {
		t.Fatalf("NewInvocationBudget: %v", err)
	}
	ctx := context.Background()

	if err := b.RecordCost(ctx, 1.20); err != nil {
		t.Fatalf("RecordCost(1.20) failed: %v", err)
	}

	// Recording in micros: 500,000 micros = $0.50 -> total $1.70
	if err := b.RecordCostMicros(ctx, 500000); err != nil {
		t.Fatalf("RecordCostMicros(500000) failed: %v", err)
	}

	// Another $0.50 exceeds $2.00
	err = b.RecordCost(ctx, 0.50)
	wantCode(t, err, ErrorCodeBudgetExceeded)
}

func TestInvocationBudget_RemainingAndConsumed(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	b, err := NewInvocationBudget(BudgetParams{
		Deadline:         now.Add(60 * time.Second),
		MaxAttempts:      5,
		RetryDelayBudget: 30 * time.Second,
		MaxOutputTokens:  2000,
		CostCeilingUSD:   5.00,
		Now:              func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewInvocationBudget: %v", err)
	}
	ctx := context.Background()

	_ = b.RecordAttempt(ctx)
	_ = b.RecordAttempt(ctx)
	_, _ = b.ReserveDelay(ctx, 10*time.Second)
	_ = b.RecordTokens(ctx, 800)
	_ = b.RecordCost(ctx, 2.00)

	attempts, delay, tokens, cost := b.Consumed()
	if attempts != 2 || delay != 10*time.Second || tokens != 800 || cost != 2.00 {
		t.Errorf("Consumed() = (%d, %v, %d, %f), want (2, 10s, 800, 2.00)", attempts, delay, tokens, cost)
	}

	remAttempts, remDelay, remTokens, remCost := b.Remaining(now)
	if remAttempts != 3 || remDelay != 20*time.Second || remTokens != 1200 || remCost != 3.00 {
		t.Errorf("Remaining() = (%d, %v, %d, %f), want (3, 20s, 1200, 3.00)", remAttempts, remDelay, remTokens, remCost)
	}

	remDeadline := b.RemainingDeadline(now)
	if remDeadline != 60*time.Second {
		t.Errorf("RemainingDeadline() = %v, want 60s", remDeadline)
	}
}

func TestInvocationBudget_ContextHelpers(t *testing.T) {
	ctx := context.Background()
	if BudgetFromContext(ctx) != nil {
		t.Fatalf("expected nil budget from empty context")
	}

	b, _ := NewInvocationBudget(BudgetParams{MaxAttempts: 2})
	ctxWithBudget := WithInvocationBudget(ctx, b)
	if BudgetFromContext(ctxWithBudget) != b {
		t.Fatalf("BudgetFromContext did not return injected budget")
	}

	// With nil budget, returns original context
	if WithInvocationBudget(ctx, nil) != ctx {
		t.Fatalf("WithInvocationBudget(ctx, nil) should return same ctx")
	}
}

func TestInvocationBudget_ConcurrencyRaceFree(t *testing.T) {
	b, err := NewInvocationBudget(BudgetParams{
		MaxAttempts:      1000,
		RetryDelayBudget: 100 * time.Second,
		MaxOutputTokens:  100000,
		CostCeilingUSD:   100.0,
	})
	if err != nil {
		t.Fatalf("NewInvocationBudget: %v", err)
	}

	ctx := context.Background()
	var wg sync.WaitGroup
	workers := 50
	iterations := 20

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				_ = b.CheckActive(ctx)
				_ = b.CanAttempt(ctx)
				_ = b.RecordAttempt(ctx)
				_, _ = b.ReserveDelay(ctx, time.Millisecond)
				_ = b.RecordTokens(ctx, 10)
				_ = b.RecordCost(ctx, 0.01)
				_ = b.Attempts()
				_, _, _, _ = b.Consumed()
				_, _, _, _ = b.Remaining(time.Now())
			}
		}()
	}
	wg.Wait()
}

func TestRetryHTTP_EnforcesBudgetAttemptsAndDelay(t *testing.T) {
	budget, err := NewInvocationBudget(BudgetParams{
		MaxAttempts:      2,
		RetryDelayBudget: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewInvocationBudget: %v", err)
	}

	ctx := context.Background()
	cfg := RetryConfig{
		MaxRetries: 5,
		BaseDelay:  10 * time.Millisecond,
		Budget:     budget,
		Sleep: func(_ context.Context, _ time.Duration) error {
			return nil
		},
	}

	attemptCount := 0
	resp, err := retryHTTP(ctx, cfg, func() (*http.Response, error) {
		attemptCount++
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Header:     http.Header{},
			Body:       http.NoBody,
		}, nil
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}

	wantCode(t, err, ErrorCodeBudgetExceeded)
	if attemptCount != 2 {
		t.Errorf("attemptCount = %d, want 2 (budget ceiling)", attemptCount)
	}
}

func TestRetryHTTP_EnforcesDelayBudgetExhaustion(t *testing.T) {
	budget, err := NewInvocationBudget(BudgetParams{
		MaxAttempts:      10,
		RetryDelayBudget: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewInvocationBudget: %v", err)
	}

	ctx := context.Background()
	cfg := RetryConfig{
		MaxRetries: 5,
		BaseDelay:  40 * time.Millisecond,
		MaxDelay:   100 * time.Millisecond,
		Budget:     budget,
		Sleep: func(_ context.Context, _ time.Duration) error {
			return nil
		},
	}

	resp, err := retryHTTP(ctx, cfg, func() (*http.Response, error) {
		// Return 429 so it attempts to retry with delay
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Retry-After": []string{"1"}}, // 1 second delay
			Body:       http.NoBody,
		}, nil
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}

	wantCode(t, err, ErrorCodeBudgetExceeded)
}
