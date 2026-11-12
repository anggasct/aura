package schedule

import (
	"context"
	"sync"
	"testing"
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

func TestObserver_FireSettleAndRetry(t *testing.T) {
	inBubble(t, nil, func(t *testing.T, f *runFixture) {
		recorder := &recordingObserver{}
		f.runner.observer = recorder.observe
		f.turns.scripts = []turnScript{
			{err: Errorf(ErrorCodeRuntimeOverloaded, "full")},
			{result: TurnResult{TurnID: "turn-2", Text: "ok", Succeeded: true}},
		}
		f.succeed(t, f.scheduled(t, f.nextFire(t)))
		counts := recorder.results()
		if counts[ResultFired] < 1 {
			t.Errorf("fired events = %v", counts)
		}
		if counts[ResultSettled] < 1 {
			t.Errorf("settled events = %v", counts)
		}
		if counts[ResultRetried] != 1 {
			t.Errorf("retried events = %v, want exactly 1", counts)
		}
		for _, event := range recorder.events {
			if event.Result == ResultFired && event.Lag < 0 {
				t.Errorf("negative lag: %+v", event)
			}
			if event.Result == ResultSettled && event.State != OccurrenceCompleted {
				t.Errorf("settle observation = %+v", event)
			}
			if event.Result == ResultSettled && event.Age < 0 {
				t.Errorf("negative age: %+v", event)
			}
		}
	})
}
