package context

import (
	"sync"
	"testing"

	stdcontext "context"
)

func TestConcurrentResolveSingleLogicalResult(t *testing.T) {
	store := newFakeSummaryStore()
	service, err := NewService(&fakeSummarizer{text: validSummaryJSON(), producer: testProducer()}, store, "compression", "v1", 32768, 2048, exactCounter{}, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	groups := summaryGroups()
	target := summaryRange()
	const workers = 8
	ids := make([]string, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			summary, err := service.ResolveRange(stdcontext.Background(), "sess-1", target, groups)
			if err != nil {
				errs[w] = err
				return
			}
			ids[w] = summary.ID
		}()
	}
	wg.Wait()
	for w := range workers {
		if errs[w] != nil {
			t.Fatalf("worker %d: %v", w, errs[w])
		}
		if ids[w] != ids[0] {
			t.Fatalf("worker %d id %q differs from %q", w, ids[w], ids[0])
		}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	matching := 0
	for _, record := range store.records {
		if record.SessionID == "sess-1" {
			matching++
		}
	}
	if matching != 1 {
		t.Errorf("stored %d records for one logical summary", matching)
	}
}

func TestConcurrentPlanSelectDeterministic(t *testing.T) {
	planner, err := NewPlanner(100, 0.80, exactCounter{})
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}
	events := []Event{
		historyTurn(1, "t1", "alpha"),
		historyTurn(2, "t2", "beta"),
		{Sequence: 3, TurnID: "t3", InvocationID: "inv-new", Kind: "model.delta", Text: "live"},
	}
	plan, err := planner.Plan(Capability{ID: "m", ContextTokens: 100000}, Invocation{InvocationID: "inv-new"}, events)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	policy, err := ResolveSelectionPolicy(2, 0.90, 64)
	if err != nil {
		t.Fatalf("ResolveSelectionPolicy: %v", err)
	}
	const workers = 8
	results := make([]*Selection, workers)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			selection, err := Select(policy, plan, exactCounter{})
			if err != nil {
				t.Error(err)
				return
			}
			results[w] = selection
		}()
	}
	wg.Wait()
	for w := 1; w < workers; w++ {
		if results[w] == nil || results[0] == nil {
			t.Fatalf("worker %d missing result", w)
		}
		if results[w].UsedTokens != results[0].UsedTokens || len(results[w].Parts) != len(results[0].Parts) {
			t.Fatalf("worker %d diverged", w)
		}
	}
}

func TestRestartReusesStoredSummaries(t *testing.T) {
	store := newFakeSummaryStore()
	firstCalls := &fakeSummarizer{text: validSummaryJSON(), producer: testProducer()}
	first, err := NewService(firstCalls, store, "compression", "v1", 32768, 2048, exactCounter{}, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	produced, err := first.ResolveRange(stdcontext.Background(), "sess-1", summaryRange(), summaryGroups())
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	secondCalls := &fakeSummarizer{text: validSummaryJSON(), producer: testProducer()}
	second, err := NewService(secondCalls, store, "compression", "v1", 32768, 2048, exactCounter{}, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	reused, err := second.ResolveRange(stdcontext.Background(), "sess-1", summaryRange(), summaryGroups())
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if secondCalls.callCount() != 0 {
		t.Errorf("restart reproduced instead of reusing: calls=%d", secondCalls.callCount())
	}
	if reused.ID != produced.ID {
		t.Errorf("ids %q vs %q", reused.ID, produced.ID)
	}
}

func TestFallbackTriggersFreshPlan(t *testing.T) {
	exactPlanner, err := NewPlanner(100, 0.80, CounterForTokenizer("test-tokenizer-v1", exactCounter{}))
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}
	estimatedPlanner, err := NewPlanner(100, 0.80, CounterForTokenizer("", exactCounter{}))
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}
	events := []Event{historyTurn(1, "t1", "work")}
	primary, err := exactPlanner.Plan(Capability{ID: "primary-model", ContextTokens: 100000, Tokenizer: "test-tokenizer-v1"}, Invocation{}, events)
	if err != nil {
		t.Fatalf("primary: %v", err)
	}
	fallback, err := estimatedPlanner.Plan(Capability{ID: "fallback-model", ContextTokens: 32000}, Invocation{}, events)
	if err != nil {
		t.Fatalf("fallback: %v", err)
	}
	if primary.Budget.Usable == fallback.Budget.Usable {
		t.Errorf("fresh plan kept the old budget")
	}
	if fallback.CapabilityID != "fallback-model" || fallback.Budget.AccountingClass != AccountingEstimated {
		t.Errorf("fallback plan = %+v", fallback.Budget)
	}
	if primary.Budget.AccountingClass != AccountingExact {
		t.Errorf("primary plan = %+v", primary.Budget)
	}
}

func TestCounterForTokenizer(t *testing.T) {
	if got := CounterForTokenizer("test-tokenizer-v1", exactCounter{}); got.Class() != AccountingExact {
		t.Errorf("pinned tokenizer did not select the exact counter")
	}
	if got := CounterForTokenizer("", exactCounter{}); got.Class() != AccountingEstimated {
		t.Errorf("unknown tokenizer did not fall back to the estimator")
	}
	if got := CounterForTokenizer("test-tokenizer-v1", nil); got.Class() != AccountingEstimated {
		t.Errorf("missing counter did not fall back to the estimator")
	}
}
