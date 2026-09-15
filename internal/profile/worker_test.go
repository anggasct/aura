package profile

import (
	"errors"
	"sync"
	"testing"
	"time"

	stdcontext "context"
)

type fakeCaller struct {
	mu      sync.Mutex
	results []*ExtractResult
	errs    []error
	calls   int
	last    *ExtractRequest
}

func (f *fakeCaller) Extract(_ stdcontext.Context, req *ExtractRequest) (*ExtractResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.last = req
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return nil, err
	}
	if len(f.results) == 0 {
		return nil, errors.New("no scripted result")
	}
	result := f.results[0]
	f.results = f.results[1:]
	return result, nil
}

func testExtractor(t *testing.T, caller ModelCaller, capacity int) (*Extractor, *fakeRegistry) {
	t.Helper()
	registry := newFakeRegistry()
	service, err := NewService(registry, Config{MinConfidence: 0.70})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	extractor, err := NewExtractor(service, caller, &stubScanner{}, "profiling", "v1", 2048, capacity, nil)
	if err != nil {
		t.Fatalf("NewExtractor: %v", err)
	}
	return extractor, registry
}

func runExtractor(t *testing.T, extractor *Extractor, registry *fakeRegistry, job Job) {
	t.Helper()
	ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
	defer cancel()
	extractor.Start(ctx)
	if err := extractor.Enqueue(job); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		registry.mu.Lock()
		count := len(registry.facts)
		registry.mu.Unlock()
		if count > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("extraction produced no facts")
}

func TestExtractorEndToEnd(t *testing.T) {
	caller := &fakeCaller{results: []*ExtractResult{{
		Text:     `[{"category":"language","key":"backend","value":"Go","sources":[3]}]`,
		Producer: ModelProducer{Provider: "p", Model: "m", ModelVersion: "v1"},
	}}}
	extractor, registry := testExtractor(t, caller, 64)
	runExtractor(t, extractor, registry, Job{
		OwnerID: "owner-1", SessionID: "sess-1",
		Events: []JobEvent{
			{ID: "evt-1", Sequence: 1, Text: "hello"},
			{ID: "evt-2", Sequence: 3, Text: "i write all backends in Go"},
		},
	})
	stored, found, err := registry.Get(stdcontext.Background(), FactID("owner-1", "language", "backend", ValueDigest("Go")))
	if err != nil || !found {
		t.Fatalf("Get: %v, %v", err, found)
	}
	if stored.Value != "Go" || stored.Origin != OriginDerived {
		t.Errorf("fact = %+v", stored)
	}
	evidence, err := registry.Evidence(stdcontext.Background(), stored.ID)
	if err != nil || len(evidence) != 1 || evidence[0].SourceEventID != "evt-2" {
		t.Errorf("evidence = %+v, %v", evidence, err)
	}
	if caller.last.Task != "profiling" || len(caller.last.Sources) != 2 {
		t.Errorf("request = %+v", caller.last)
	}
}

func TestExtractorScreensSensitive(t *testing.T) {
	caller := &fakeCaller{results: []*ExtractResult{{
		Text:     `[{"category":"tool","key":"db password","value":"hunter2","sources":[1]},{"category":"language","key":"backend","value":"Go","sources":[1]}]`,
		Producer: ModelProducer{Provider: "p", Model: "m"},
	}}}
	extractor, registry := testExtractor(t, caller, 64)
	runExtractor(t, extractor, registry, Job{
		OwnerID: "owner-1", SessionID: "sess-1",
		Events: []JobEvent{{ID: "evt-1", Sequence: 1, Text: "my db password is hunter2 but i write Go"}},
	})
	registry.mu.Lock()
	count := len(registry.facts)
	registry.mu.Unlock()
	if count != 1 {
		t.Errorf("facts = %d, want only the clean candidate", count)
	}
}

func TestExtractorRejectsUnknownSources(t *testing.T) {
	caller := &fakeCaller{results: []*ExtractResult{{
		Text:     `[{"category":"language","key":"backend","value":"Go","sources":[99]}]`,
		Producer: ModelProducer{Provider: "p", Model: "m"},
	}}}
	extractor, registry := testExtractor(t, caller, 64)
	ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
	defer cancel()
	extractor.Start(ctx)
	if err := extractor.Enqueue(Job{
		OwnerID: "owner-1", SessionID: "sess-1",
		Events: []JobEvent{{ID: "evt-1", Sequence: 1, Text: "hello"}},
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		caller.mu.Lock()
		calls := caller.calls
		caller.mu.Unlock()
		if calls > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	registry.mu.Lock()
	count := len(registry.facts)
	registry.mu.Unlock()
	if count != 0 {
		t.Errorf("facts = %d, want none for unknown sources", count)
	}
}

func TestExtractorRetriesThenDeadLetters(t *testing.T) {
	caller := &fakeCaller{errs: []error{errors.New("boom"), errors.New("boom"), errors.New("boom")}}
	extractor, _ := testExtractor(t, caller, 64)
	ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
	defer cancel()
	extractor.Start(ctx)
	if err := extractor.Enqueue(Job{
		OwnerID: "owner-1", SessionID: "sess-1",
		Events: []JobEvent{{ID: "evt-1", Sequence: 1, Text: "hello"}},
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		caller.mu.Lock()
		calls := caller.calls
		caller.mu.Unlock()
		if calls == extractorMaxAttempts {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("retry did not stop at the bound")
}

func TestExtractorRecoversAfterTransient(t *testing.T) {
	caller := &fakeCaller{
		errs:    []error{errors.New("boom")},
		results: []*ExtractResult{{Text: `[{"category":"language","key":"backend","value":"Go","sources":[1]}]`, Producer: ModelProducer{Provider: "p", Model: "m"}}},
	}
	extractor, registry := testExtractor(t, caller, 64)
	runExtractor(t, extractor, registry, Job{
		OwnerID: "owner-1", SessionID: "sess-1",
		Events: []JobEvent{{ID: "evt-1", Sequence: 1, Text: "go please"}},
	})
	caller.mu.Lock()
	calls := caller.calls
	caller.mu.Unlock()
	if calls != 2 {
		t.Errorf("calls = %d, want one retry", calls)
	}
	registry.mu.Lock()
	count := len(registry.facts)
	registry.mu.Unlock()
	if count != 1 {
		t.Errorf("facts = %d", count)
	}
}

func TestExtractorReprocessingIdempotent(t *testing.T) {
	caller := &fakeCaller{results: []*ExtractResult{
		{Text: `[{"category":"language","key":"backend","value":"Go","sources":[1]}]`, Producer: ModelProducer{Provider: "p", Model: "m"}},
		{Text: `[{"category":"language","key":"backend","value":"Go","sources":[1]}]`, Producer: ModelProducer{Provider: "p", Model: "m"}},
	}}
	extractor, registry := testExtractor(t, caller, 64)
	job := Job{
		OwnerID: "owner-1", SessionID: "sess-1",
		Events: []JobEvent{{ID: "evt-1", Sequence: 1, Text: "go please"}},
	}
	ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
	defer cancel()
	extractor.Start(ctx)
	for range 2 {
		if err := extractor.Enqueue(job); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		caller.mu.Lock()
		calls := caller.calls
		caller.mu.Unlock()
		if calls == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	evidence, err := registry.Evidence(stdcontext.Background(), FactID("owner-1", "language", "backend", ValueDigest("Go")))
	if err != nil || len(evidence) != 1 {
		t.Errorf("evidence = %+v, %v", evidence, err)
	}
}

func TestEnqueueBackpressure(t *testing.T) {
	caller := &fakeCaller{}
	extractor, _ := testExtractor(t, caller, 1)
	if err := extractor.Enqueue(Job{OwnerID: "o", SessionID: "s", Events: []JobEvent{{ID: "e1", Sequence: 1, Text: "a"}}}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	err := extractor.Enqueue(Job{OwnerID: "o", SessionID: "s", Events: []JobEvent{{ID: "e2", Sequence: 2, Text: "b"}}})
	if err == nil {
		t.Fatalf("expected queue-full rejection")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeProfileUnavailable {
		t.Fatalf("code = %v, %v (%v)", code, ok, err)
	}
	if extractor.dropped.Load() != 1 {
		t.Errorf("dropped = %d", extractor.dropped.Load())
	}
}

func TestEnqueueValidation(t *testing.T) {
	caller := &fakeCaller{}
	extractor, _ := testExtractor(t, caller, 64)
	cases := map[string]Job{
		"empty owner":   {SessionID: "s", Events: []JobEvent{{ID: "e", Sequence: 1}}},
		"empty session": {OwnerID: "o", Events: []JobEvent{{ID: "e", Sequence: 1}}},
		"no events":     {OwnerID: "o", SessionID: "s"},
		"empty id":      {OwnerID: "o", SessionID: "s", Events: []JobEvent{{Sequence: 1}}},
		"zero sequence": {OwnerID: "o", SessionID: "s", Events: []JobEvent{{ID: "e"}}},
		"duplicate":     {OwnerID: "o", SessionID: "s", Events: []JobEvent{{ID: "a", Sequence: 1}, {ID: "b", Sequence: 1}}},
	}
	for name, job := range cases {
		t.Run(name, func(t *testing.T) {
			if err := extractor.Enqueue(job); err == nil {
				t.Errorf("expected rejection")
			}
		})
	}
}

func TestNewExtractorRejects(t *testing.T) {
	service, err := NewService(newFakeRegistry(), Config{MinConfidence: 0.70})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	caller := &fakeCaller{}
	if _, err := NewExtractor(nil, caller, &stubScanner{}, "profiling", "v1", 2048, 64, nil); err == nil {
		t.Errorf("expected nil service rejection")
	}
	if _, err := NewExtractor(service, nil, &stubScanner{}, "profiling", "v1", 2048, 64, nil); err == nil {
		t.Errorf("expected nil caller rejection")
	}
	if _, err := NewExtractor(service, caller, nil, "profiling", "v1", 2048, 64, nil); err == nil {
		t.Errorf("expected nil scanner rejection")
	}
	if _, err := NewExtractor(service, caller, &stubScanner{}, "", "v1", 2048, 64, nil); err == nil {
		t.Errorf("expected empty task rejection")
	}
	if _, err := NewExtractor(service, caller, &stubScanner{}, "profiling", "v9", 2048, 64, nil); err == nil {
		t.Errorf("expected prompt rejection")
	}
	if _, err := NewExtractor(service, caller, &stubScanner{}, "profiling", "v1", 0, 64, nil); err == nil {
		t.Errorf("expected bound rejection")
	}
	if _, err := NewExtractor(service, caller, &stubScanner{}, "profiling", "v1", 2048, 0, nil); err == nil {
		t.Errorf("expected capacity rejection")
	}
}

func TestExtractorStopsOnCancel(t *testing.T) {
	blocking := &blockingCaller{release: make(chan struct{})}
	extractor, registry := testExtractor(t, blocking, 64)
	ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
	extractor.Start(ctx)
	if err := extractor.Enqueue(Job{
		OwnerID: "owner-1", SessionID: "sess-1",
		Events: []JobEvent{{ID: "evt-1", Sequence: 1, Text: "hello"}},
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	time.Sleep(100 * time.Millisecond)
	registry.mu.Lock()
	count := len(registry.facts)
	registry.mu.Unlock()
	if count != 0 {
		t.Errorf("facts = %d, want none after cancel", count)
	}
}

type blockingCaller struct {
	release chan struct{}
}

func (b *blockingCaller) Extract(ctx stdcontext.Context, _ *ExtractRequest) (*ExtractResult, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-b.release:
		return &ExtractResult{}, nil
	}
}
