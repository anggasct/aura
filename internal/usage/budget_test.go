package usage

import (
	"context"
	"errors"
	"iter"
	"sync"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

type fakeLLM struct {
	name  string
	usage *genai.GenerateContentResponseUsageMetadata
	err   error
	calls int
	mu    sync.Mutex
}

func (f *fakeLLM) Name() string { return f.name }

func (f *fakeLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if f.err != nil {
			yield(nil, f.err)
			return
		}
		yield(&adkmodel.LLMResponse{UsageMetadata: f.usage, TurnComplete: true}, nil)
	}
}

func (f *fakeLLM) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func budgetedLedger(t *testing.T, dailyCap int64) (*Ledger, *fakeLLM) {
	t.Helper()
	l := newTestLedger(t, dailyCap, 10000000)
	inner := &fakeLLM{
		name: "fake",
		usage: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     50,
			CandidatesTokenCount: 100,
		},
	}
	return l, inner
}

func TestBudgetedReservesAndSettles(t *testing.T) {
	l, inner := budgetedLedger(t, 1000000)
	b, err := NewBudgeted(inner, l, "primary")
	if err != nil {
		t.Fatal(err)
	}
	req := &adkmodel.LLMRequest{
		Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hello world"}}}},
	}
	var got *adkmodel.LLMResponse
	for resp, err := range b.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
		got = resp
	}
	if got == nil || !got.TurnComplete {
		t.Fatal("expected a complete response")
	}

	entries, err := l.Entries(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	en := entries[0]
	if en.InputTokens != 50 || en.OutputTokens != 100 {
		t.Errorf("usage = %d/%d, want 50/100", en.InputTokens, en.OutputTokens)
	}
	if en.Accounting != accountingReported {
		t.Errorf("accounting = %q, want reported", en.Accounting)
	}
	want := testPrice("primary").CostMicros(Usage{InputTokens: 50, OutputTokens: 100})
	if en.CostMicros != want {
		t.Errorf("cost = %d, want %d", en.CostMicros, want)
	}
}

func TestBudgetedBlocksOnBudgetExhaustion(t *testing.T) {
	l, inner := budgetedLedger(t, 100) // too small for any reservation
	b, err := NewBudgeted(inner, l, "primary")
	if err != nil {
		t.Fatal(err)
	}
	req := &adkmodel.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hello"}}}}}
	var sawErr error
	for _, err := range b.GenerateContent(context.Background(), req, false) {
		if err != nil {
			sawErr = err
		}
	}
	if code, ok := CodeOf(sawErr); !ok || code != ErrorCodeBudgetExceeded {
		t.Errorf("code = %v, want budget_exceeded (err=%v)", code, sawErr)
	}
	if inner.callCount() != 0 {
		t.Errorf("inner called %d times, want 0 (budget must block before dispatch)", inner.callCount())
	}
}

func TestBudgetedSettlesEstimatedOnError(t *testing.T) {
	l, inner := budgetedLedger(t, 1000000)
	inner.err = errors.New("provider down")
	b, err := NewBudgeted(inner, l, "primary")
	if err != nil {
		t.Fatal(err)
	}
	req := &adkmodel.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hello"}}}}}
	var sawErr error
	for _, err := range b.GenerateContent(context.Background(), req, false) {
		if err != nil {
			sawErr = err
		}
	}
	if sawErr == nil {
		t.Fatal("expected the provider error to pass through")
	}
	entries, err := l.Entries(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].Accounting != accountingEstimated {
		t.Errorf("accounting = %q, want estimated on error", entries[0].Accounting)
	}
	if entries[0].CostMicros < 1 {
		t.Errorf("cost = %d, want >= 1 (conservative, never zero)", entries[0].CostMicros)
	}
}

func TestBudgetedNilLedgerPassthrough(t *testing.T) {
	inner := &fakeLLM{name: "fake"}
	got, err := NewBudgeted(inner, nil, "primary")
	if err != nil {
		t.Fatal(err)
	}
	if got != adkmodel.LLM(inner) {
		t.Errorf("nil ledger must return inner unchanged")
	}
}

func TestBudgetedNilInnerRejected(t *testing.T) {
	l := newTestLedger(t, 1000000, 10000000)
	if _, err := NewBudgeted(nil, l, "primary"); err == nil {
		t.Error("expected error for nil inner")
	}
}

func TestBudgetedConcurrentDispatch(t *testing.T) {
	l, inner := budgetedLedger(t, 10000000)
	b, err := NewBudgeted(inner, l, "primary")
	if err != nil {
		t.Fatal(err)
	}
	req := &adkmodel.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hello"}}}}}

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, err := range b.GenerateContent(context.Background(), req, false) {
				if err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent dispatch error: %v", err)
	}
	if inner.callCount() != 20 {
		t.Errorf("inner calls = %d, want 20", inner.callCount())
	}
	entries, err := l.Entries(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 20 {
		t.Errorf("entries = %d, want 20 (each dispatch settled exactly once)", len(entries))
	}
}
