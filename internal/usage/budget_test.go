package usage

import (
	"context"
	"database/sql"
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
	b, err := NewBudgeted(inner, l, "primary", nil)
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
	b, err := NewBudgeted(inner, l, "primary", nil)
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
	b, err := NewBudgeted(inner, l, "primary", nil)
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
	got, err := NewBudgeted(inner, nil, "primary", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != adkmodel.LLM(inner) {
		t.Errorf("nil ledger must return inner unchanged")
	}
}

func TestBudgetedNilInnerRejected(t *testing.T) {
	l := newTestLedger(t, 1000000, 10000000)
	if _, err := NewBudgeted(nil, l, "primary", nil); err == nil {
		t.Error("expected error for nil inner")
	}
}

func TestBudgetedConcurrentDispatch(t *testing.T) {
	l, inner := budgetedLedger(t, 10000000)
	b, err := NewBudgeted(inner, l, "primary", nil)
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

type dbClosingLLM struct {
	db    *sql.DB
	usage *genai.GenerateContentResponseUsageMetadata
	fail  error
}

func (f *dbClosingLLM) Name() string { return "saboteur" }

func (f *dbClosingLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		_ = f.db.Close()
		if f.fail != nil {
			yield(nil, f.fail)
			return
		}
		yield(&adkmodel.LLMResponse{UsageMetadata: f.usage, TurnComplete: true}, nil)
	}
}

func TestBudgetedPropagatesSettlementFailure(t *testing.T) {
	l, db := newTestLedgerWithDB(t, 1000000, 10000000)
	inner := &dbClosingLLM{
		db: db,
		usage: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     10,
			CandidatesTokenCount: 20,
		},
	}
	b, err := NewBudgeted(inner, l, "primary", nil)
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
		t.Fatal("expected the settlement failure to be surfaced, got nil")
	}
}

func TestBudgetedJoinsProviderAndSettlementErrors(t *testing.T) {
	l, db := newTestLedgerWithDB(t, 1000000, 10000000)
	providerErr := errors.New("provider exploded")
	inner := &dbClosingLLM{db: db, fail: providerErr}
	b, err := NewBudgeted(inner, l, "primary", nil)
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
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(sawErr, providerErr) {
		t.Errorf("provider error must be preserved, got %v", sawErr)
	}
}

type errAfterResponseLLM struct {
	providerErr error
}

func (f *errAfterResponseLLM) Name() string { return "err-after-response" }

func (f *errAfterResponseLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(&adkmodel.LLMResponse{UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     10,
			CandidatesTokenCount: 20,
		}, TurnComplete: true}, nil)
		yield(nil, f.providerErr)
	}
}

func TestBudgetedErrorPathYieldsFinalResponse(t *testing.T) {
	l, _ := budgetedLedger(t, 1000000)
	providerErr := errors.New("provider exploded mid-stream")

	b, err := NewBudgeted(&errAfterResponseLLM{providerErr: providerErr}, l, "primary", nil)
	if err != nil {
		t.Fatal(err)
	}
	req := &adkmodel.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hello"}}}}}

	var sawErr error
	var errResp *adkmodel.LLMResponse
	for resp, err := range b.GenerateContent(context.Background(), req, false) {
		if err != nil {
			sawErr = err
			errResp = resp
		}
	}
	if sawErr == nil || !errors.Is(sawErr, providerErr) {
		t.Fatalf("provider error must be surfaced, got %v", sawErr)
	}
	if errResp == nil {
		t.Fatal("the response attached to the provider error must be yielded, got nil")
	}
	if errResp.UsageMetadata == nil || errResp.UsageMetadata.PromptTokenCount != 10 {
		t.Errorf("error-attached response usage = %+v, want prompt_token_count 10", errResp.UsageMetadata)
	}
}

type streamStopsLLM struct{}

func (f *streamStopsLLM) Name() string { return "stream-stops" }

func (f *streamStopsLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(&adkmodel.LLMResponse{UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     10,
			CandidatesTokenCount: 20,
		}, TurnComplete: false}, nil)
	}
}

type noCompleteLLM struct{}

func (f *noCompleteLLM) Name() string { return "no-complete" }

func (f *noCompleteLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(&adkmodel.LLMResponse{UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     10,
			CandidatesTokenCount: 20,
		}, TurnComplete: false}, nil)
	}
}

func TestBudgetedConsumerStopSettlesConservative(t *testing.T) {
	l := newTestLedger(t, 10000000, 10000000)
	b, err := NewBudgeted(&streamStopsLLM{}, l, "primary", nil)
	if err != nil {
		t.Fatal(err)
	}
	req := &adkmodel.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hello"}}}}}
	for resp := range b.GenerateContent(context.Background(), req, false) {
		_ = resp
		break
	}

	entries, err := l.Entries(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].Accounting != accountingEstimated {
		t.Errorf("consumer-stop accounting = %q, want estimated (conservative)", entries[0].Accounting)
	}
	if entries[0].CostMicros < 100 {
		t.Errorf("consumer-stop cost = %d, want >= reserved cost (conservative, not partial)", entries[0].CostMicros)
	}
}

func TestBudgetedCleanExhaustionWithoutCompleteSettlesConservative(t *testing.T) {
	l := newTestLedger(t, 10000000, 10000000)
	b, err := NewBudgeted(&noCompleteLLM{}, l, "primary", nil)
	if err != nil {
		t.Fatal(err)
	}
	req := &adkmodel.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hello"}}}}}
	for range b.GenerateContent(context.Background(), req, false) {
	}

	entries, err := l.Entries(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].Accounting != accountingEstimated {
		t.Errorf("no-complete accounting = %q, want estimated (conservative)", entries[0].Accounting)
	}
	if entries[0].CostMicros < 100 {
		t.Errorf("no-complete cost = %d, want >= reserved cost (conservative, not partial)", entries[0].CostMicros)
	}
}

func TestBudgetedSettlesCacheAndReasoningTokens(t *testing.T) {
	l, inner := budgetedLedger(t, 1000000)
	inner.usage = &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:        50,
		CandidatesTokenCount:    100,
		CachedContentTokenCount: 30,
		ThoughtsTokenCount:      7,
	}
	b, err := NewBudgeted(inner, l, "primary", nil)
	if err != nil {
		t.Fatal(err)
	}
	req := &adkmodel.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hello"}}}}}
	for range b.GenerateContent(context.Background(), req, false) {
	}

	entries, err := l.Entries(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	want := testPrice("primary").CostMicros(Usage{InputTokens: 50, OutputTokens: 100, CacheTokens: 30, ReasoningTokens: 7})
	if entries[0].CostMicros != want {
		t.Errorf("cost = %d, want %d (cache/reasoning must be priced, not dropped)", entries[0].CostMicros, want)
	}
	if entries[0].CostMicros <= testPrice("primary").CostMicros(Usage{InputTokens: 50, OutputTokens: 100}) {
		t.Errorf("cost = %d, want strictly more than input+output-only cost (cache/reasoning ignored)", entries[0].CostMicros)
	}
}

func TestBudgetedCancelledContextBlocksDispatch(t *testing.T) {
	l, inner := budgetedLedger(t, 1000000)
	b, err := NewBudgeted(inner, l, "primary", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := &adkmodel.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hello"}}}}}

	var sawErr error
	for _, err := range b.GenerateContent(ctx, req, false) {
		if err != nil {
			sawErr = err
		}
	}
	if sawErr == nil {
		t.Fatal("expected a cancellation error before dispatch")
	}
	if inner.callCount() != 0 {
		t.Errorf("inner called %d times, want 0 (cancelled before dispatch)", inner.callCount())
	}
}

func TestBudgetedIdempotentReplay(t *testing.T) {
	l, inner := budgetedLedger(t, 1000000)
	b, err := NewBudgeted(inner, l, "primary", nil)
	if err != nil {
		t.Fatal(err)
	}
	req := &adkmodel.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hello"}}}}}
	ctx := WithInvocation(context.Background(), "inv-replay", 0)

	for range 2 {
		for _, err := range b.GenerateContent(ctx, req, false) {
			if err != nil {
				t.Fatalf("replay dispatch: %v", err)
			}
		}
	}

	entries, err := l.Entries(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("entries = %d, want 1 (replay must not duplicate settlement)", len(entries))
	}
	var reservations int
	if err := l.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM usage_reservation").Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if reservations != 1 {
		t.Errorf("reservations = %d, want 1 (replay must not duplicate the reservation)", reservations)
	}
}

func TestEstimateInputTokensAccountsAllParts(t *testing.T) {
	textOnly, err := estimateInputTokens(&adkmodel.LLMRequest{
		Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hello"}}}},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	withInline, err := estimateInputTokens(&adkmodel.LLMRequest{
		Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{
			{Text: "hello"},
			{InlineData: &genai.Blob{Data: make([]byte, 4000), MIMEType: "image/png"}},
		}}},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if withInline <= textOnly {
		t.Errorf("inline data must raise the estimate: inline=%d text=%d", withInline, textOnly)
	}
	if withInline-textOnly < 900 {
		t.Errorf("inline data delta = %d, want ~1000 tokens for 4000 bytes", withInline-textOnly)
	}
}

func TestEstimateInputTokensRejectsFileDataUnderCap(t *testing.T) {
	req := &adkmodel.LLMRequest{
		Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{
			{FileData: &genai.FileData{FileURI: "gs://bucket/big.mp4", MIMEType: "video/mp4"}},
		}}},
	}
	if _, err := estimateInputTokens(req, true); err == nil {
		t.Error("strict estimate must reject file_data whose size is unknown")
	}
	if _, err := estimateInputTokens(req, false); err != nil {
		t.Errorf("non-strict estimate rejected file_data: %v", err)
	}
}

func TestBudgetedRejectsFileDataUnderCap(t *testing.T) {
	l, inner := budgetedLedger(t, 1000000)
	b, err := NewBudgeted(inner, l, "primary", nil)
	if err != nil {
		t.Fatal(err)
	}
	req := &adkmodel.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{
		{FileData: &genai.FileData{FileURI: "gs://bucket/media.png", MIMEType: "image/png"}},
	}}}}
	var sawErr error
	for _, err := range b.GenerateContent(context.Background(), req, false) {
		if err != nil {
			sawErr = err
		}
	}
	if sawErr == nil {
		t.Fatal("expected file_data under a cap to be rejected before dispatch")
	}
	if code, ok := CodeOf(sawErr); !ok || code != ErrorCodeInvalidArgument {
		t.Errorf("code = %v, want invalid_argument (err=%v)", code, sawErr)
	}
	if inner.callCount() != 0 {
		t.Errorf("inner called %d times, want 0 (rejected before dispatch)", inner.callCount())
	}
}
