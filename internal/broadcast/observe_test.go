package broadcast

import (
	"context"
	"sync"
	"testing"
	"time"
)

type recordingObserver struct {
	mu     sync.Mutex
	events []Observation
}

func (r *recordingObserver) observe(_ context.Context, observation *Observation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, *observation)
}

func (r *recordingObserver) results() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	counts := map[string]int{}
	for _, event := range r.events {
		counts[event.Result]++
	}
	return counts
}

func (r *recordingObserver) find(result string) (Observation, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, event := range r.events {
		if event.Result == result {
			return event, true
		}
	}
	return Observation{}, false
}

func TestObserver_SubmitHeldAndScheduled(t *testing.T) {
	recorder := &recordingObserver{}
	store := newFakeItemStore()
	broadcaster, err := New(testPolicy(), &fakeRegistry{registered: map[string]bool{"discord": true}}, store, nil, recorder.observe)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	quiet := testPolicy()
	quiet.Quiet = QuietConfig{Enabled: true, Location: time.UTC, StartMin: 1380, EndMin: 420}
	held, err := New(quiet, &fakeRegistry{registered: map[string]bool{"discord": true}}, newFakeItemStore(), nil, recorder.observe)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	notification := testNotification()
	if _, _, err := broadcaster.Submit(t.Context(), notification); err != nil {
		t.Fatalf("Submit(): %v", err)
	}
	night := testNotification()
	night.IdempotencyKey = "night-key"
	night.CreatedAt = time.Date(2026, 9, 12, 23, 30, 0, 0, time.UTC)
	if _, _, err := held.Submit(t.Context(), night); err != nil {
		t.Fatalf("Submit(): %v", err)
	}
	counts := recorder.results()
	if counts[ResultSubmitted] != 2 {
		t.Errorf("submitted events = %v", counts)
	}
}

func TestObserver_DeliveryLifecycle(t *testing.T) {
	recorder := &recordingObserver{}
	fixture := newRunnerFixture([]sendScript{successOutcome("intent-1")})
	fixture.runner.observer = recorder.observe
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	seedRunItem(fixture, "bcst-1", StateScheduled, now)
	startBroadcastRun(t, fixture, "bcst-1")
	waitBroadcastState(t, fixture, "bcst-1", StateSucceeded)
	delivered, ok := recorder.find(ResultDelivered)
	if !ok {
		t.Fatal("no delivered observation")
	}
	if delivered.Priority != PriorityWarning || delivered.State != StateSucceeded || delivered.Attempts != 1 {
		t.Errorf("delivered observation = %+v", delivered)
	}
	if delivered.Age <= 0 {
		t.Errorf("delivered age = %v, want positive", delivered.Age)
	}
	for _, event := range recorder.events {
		if event.Result == ResultSubmitted {
			t.Errorf("runner emitted submit observation: %+v", event)
		}
	}
}

func TestObserver_RetryFallbackAndRateDelay(t *testing.T) {
	recorder := &recordingObserver{}
	policy := testRunPolicy()
	policy.DispatchGap = time.Minute
	policy.Fallback = map[string]string{"default": "discord:fallback"}
	policy.MaxAttempts = 1
	fixture := newRunnerFixtureWithPolicy(policy, []sendScript{{outcome: SendOutcome{IntentID: "intent-bad"}}})
	fixture.runner.observer = recorder.observe
	now := time.Now().UTC()
	if _, err := fixture.store.ClaimDispatchSlot(t.Context(), "default", now.Add(time.Hour), time.Minute); err != nil {
		t.Fatalf("pre-claim: %v", err)
	}
	seedRunItem(fixture, "bcst-1", StateScheduled, now)
	fallbackSender := &fakeSender{scripts: []sendScript{successOutcome("intent-fb")}}
	fixture.runner.senders["discord"] = &routeSwitchSender{primary: fixture.sender, fallback: fallbackSender}
	startBroadcastRun(t, fixture, "bcst-1")
	waitBroadcastState(t, fixture, "bcst-1", StateSucceeded)
	counts := recorder.results()
	for _, want := range []string{ResultRateDelayed, ResultFallback, ResultDelivered} {
		if counts[want] != 1 {
			t.Errorf("event %q count = %d, want 1 (%v)", want, counts[want], counts)
		}
	}
	delayed, ok := recorder.find(ResultRateDelayed)
	if !ok || delayed.RateDelay <= 0 {
		t.Errorf("rate delay observation = %+v, %v", delayed, ok)
	}
}
