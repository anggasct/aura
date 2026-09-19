package profile

import (
	stdcontext "context"
	"errors"
	"sync"
	"testing"
	"time"
)

type recordingObserver struct {
	mu           sync.Mutex
	observations []Observation
}

func (r *recordingObserver) observe(_ stdcontext.Context, observation *Observation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observations = append(r.observations, *observation)
}

func (r *recordingObserver) snapshot() []Observation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Observation(nil), r.observations...)
}

func (r *recordingObserver) waitForResult(t *testing.T, result string) Observation {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, observation := range r.snapshot() {
			if observation.Result == result {
				return observation
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("observation with result %q never arrived: %+v", result, r.snapshot())
	return Observation{}
}

func TestExtractorObservesResults(t *testing.T) {
	observer := &recordingObserver{}
	registry := newFakeRegistry()
	service, err := NewService(registry, Config{MinConfidence: 0.70})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	caller := &fakeCaller{results: []*ExtractResult{{
		Text:     `[{"category":"tool","key":"editor","value":"neovim","sources":[1]}]`,
		Producer: ModelProducer{Provider: "test", Model: "m1"},
	}}}
	extractor, err := NewExtractor(service, caller, &stubScanner{}, "profiling", "v1", 2048, 8, nil, WithObserver(observer.observe))
	if err != nil {
		t.Fatalf("NewExtractor: %v", err)
	}
	ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
	defer cancel()
	extractor.Start(ctx)
	if err := extractor.Enqueue(Job{
		OwnerID: "owner-1", SessionID: "sess-1",
		Events: []JobEvent{{ID: "evt-1", Sequence: 1, Text: "i edit with neovim"}},
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	completed := observer.waitForResult(t, ResultCompleted)
	if completed.Kind != ObserveExtraction || completed.Accepted != 1 {
		t.Errorf("completed observation = %+v", completed)
	}
	if completed.QueueAge < 0 {
		t.Errorf("queue age = %v, want non-negative", completed.QueueAge)
	}
}

func TestExtractorObservesDeadLetter(t *testing.T) {
	observer := &recordingObserver{}
	registry := newFakeRegistry()
	service, err := NewService(registry, Config{MinConfidence: 0.70})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	caller := &fakeCaller{errs: []error{
		errors.New("boom"), errors.New("boom"), errors.New("boom"),
	}}
	extractor, err := NewExtractor(service, caller, &stubScanner{}, "profiling", "v1", 2048, 8, nil, WithObserver(observer.observe))
	if err != nil {
		t.Fatalf("NewExtractor: %v", err)
	}
	ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
	defer cancel()
	extractor.Start(ctx)
	if err := extractor.Enqueue(Job{
		OwnerID: "owner-1", SessionID: "sess-1",
		Events: []JobEvent{{ID: "evt-1", Sequence: 1, Text: "hello"}},
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	failed := observer.waitForResult(t, ResultFailed)
	if failed.Kind != ObserveExtraction || failed.Accepted != 0 {
		t.Errorf("failed observation = %+v", failed)
	}
}

func TestExtractorObservesDroppedJob(t *testing.T) {
	observer := &recordingObserver{}
	registry := newFakeRegistry()
	service, err := NewService(registry, Config{MinConfidence: 0.70})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	caller := &fakeCaller{}
	extractor, err := NewExtractor(service, caller, &stubScanner{}, "profiling", "v1", 2048, 1, nil, WithObserver(observer.observe))
	if err != nil {
		t.Fatalf("NewExtractor: %v", err)
	}
	if err := extractor.Enqueue(Job{
		OwnerID: "owner-1", SessionID: "sess-1",
		Events: []JobEvent{{ID: "evt-1", Sequence: 1, Text: "one"}},
	}); err != nil {
		t.Fatalf("Enqueue first: %v", err)
	}
	if err := extractor.Enqueue(Job{
		OwnerID: "owner-1", SessionID: "sess-1",
		Events: []JobEvent{{ID: "evt-2", Sequence: 1, Text: "two"}},
	}); err == nil {
		t.Fatal("second enqueue over a full queue must fail")
	}
	dropped := observer.waitForResult(t, ResultDropped)
	if dropped.Kind != ObserveExtraction {
		t.Errorf("dropped observation = %+v", dropped)
	}
	if extractor.QueueDepth() != 1 || extractor.DroppedJobs() != 1 {
		t.Errorf("queue depth = %d, dropped = %d", extractor.QueueDepth(), extractor.DroppedJobs())
	}
}

func TestExpireObservesLag(t *testing.T) {
	observer := &recordingObserver{}
	registry := newFakeRegistry()
	service, err := NewService(registry, Config{MinConfidence: 0.70, Observer: observer.observe})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	now := time.Now().UTC()
	past := now.Add(-90 * time.Minute)
	fact, err := service.ProposeFact(t.Context(), "owner-1", "timezone", "home", "WIB", testEvidence("d1"), now)
	if err != nil {
		t.Fatalf("ProposeFact: %v", err)
	}
	stored, _, _ := registry.Get(t.Context(), fact.ID)
	stored.Status = StatusActive
	stored.ExpiresAt = &past
	registry.mu.Lock()
	registry.facts[fact.ID] = &stored
	registry.mu.Unlock()
	expired, err := service.ExpireFacts(t.Context(), now)
	if err != nil || len(expired) != 1 {
		t.Fatalf("ExpireFacts: %d, %v", len(expired), err)
	}
	observations := observer.snapshot()
	if len(observations) != 1 {
		t.Fatalf("observations = %+v", observations)
	}
	if observations[0].Kind != ObserveExpiry {
		t.Errorf("kind = %q", observations[0].Kind)
	}
	if observations[0].Lag < 89*time.Minute || observations[0].Lag > 91*time.Minute {
		t.Errorf("lag = %v, want about 90m", observations[0].Lag)
	}
}
