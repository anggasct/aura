package schedule

import (
	"errors"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
)

func (h *scheduleHost) failTheFirstTurn() {
	h.turns.mu.Lock()
	defer h.turns.mu.Unlock()
	h.turns.scripts = []turnScript{{err: errors.New("connection reset")}}
}

func (h *scheduleHost) endedRecordedRun() (string, time.Time) {
	h.t.Helper()
	h.failTheFirstTurn()
	manual := h.clock.Now().Add(-time.Minute).Truncate(time.Second).Add(11 * time.Nanosecond)
	h.store.seed(&Occurrence{ID: "cron-manual", JobID: h.job.ID, ScheduledForUTC: manual, State: OccurrenceFired, CreatedAt: h.clock.Now(), UpdatedAt: h.clock.Now()})
	if err := StartFire(h.t.Context(), &FireRequest{Starter: h.runtime, JobID: h.job.ID, Instant: manual}); err != nil {
		h.t.Fatalf("StartFire(): %v", err)
	}
	key := fireKey(h.job.ID, manual)
	h.awaitState(key, durable.RunFailed)
	if state := h.store.stateAt(h.job.ID, manual); state != OccurrenceFired {
		h.t.Fatalf("occurrence after the failed turn = %q, want fired", state)
	}
	return key, manual
}

func (h *scheduleHost) startRuns(reuse durable.ReusePolicy, now time.Time) {
	h.t.Helper()
	if _, err := StartRuns(h.t.Context(), &RunsRequest{Starter: h.runtime, Store: h.store, Now: now, Reuse: reuse}); err != nil {
		h.t.Fatalf("StartRuns(): %v", err)
	}
}

func (h *scheduleHost) staysEnded(key string) {
	h.t.Helper()
	h.steady(key+" stays failed without a new turn", func() bool {
		return h.state(key) == durable.RunFailed && h.turns.callCount() == 1 && !h.log.invoked(key)
	})
}

func TestStartRunsRestartsAnEndedRunOnlyOnRequest(t *testing.T) {
	t.Run("the zero policy leaves a recorded run ended", func(t *testing.T) {
		h := newScheduleHost(t, hourly)
		key, manual := h.endedRecordedRun()
		h.log.reset()
		h.startRuns(durable.ReuseNever, h.clock.Now())
		h.staysEnded(key)
		if state := h.store.stateAt(h.job.ID, manual); state != OccurrenceFired {
			t.Errorf("occurrence = %q, want fired", state)
		}
	})
	t.Run("the reuse policy starts a recorded run again", func(t *testing.T) {
		h := newScheduleHost(t, hourly)
		key, manual := h.endedRecordedRun()
		h.startRuns(durable.ReuseEnded, h.clock.Now())
		h.awaitState(key, durable.RunSucceeded)
		requests := h.turns.requests()
		if len(requests) != 2 || requests[1].IdempotencyKey != requests[0].IdempotencyKey || requests[1].IdempotencyKey != turnKey(h.job.ID, manual) {
			t.Errorf("turn requests = %+v, want the occurrence's turn submitted again under its key", requests)
		}
		if state := h.store.stateAt(h.job.ID, manual); state != OccurrenceCompleted {
			t.Errorf("occurrence = %q, want completed", state)
		}
	})
	t.Run("a scheduled run that ended stays ended under either policy", func(t *testing.T) {
		h := newScheduleHost(t, hourly)
		h.failTheFirstTurn()
		fire := h.clock.Now().Add(time.Hour)
		key := h.startFire(fire)
		h.awaitSleeping(key, 1)
		h.clock.Advance(time.Hour)
		h.awaitState(key, durable.RunFailed)
		h.store.remove(h.job.ID, fire)
		h.log.reset()
		for _, reuse := range []durable.ReusePolicy{durable.ReuseNever, durable.ReuseEnded} {
			h.startRuns(reuse, fire.Add(-time.Second))
			h.staysEnded(key)
		}
	})
}
