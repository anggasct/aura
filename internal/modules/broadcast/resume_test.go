package broadcast

import (
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
)

const endedRunWindow = 200 * time.Millisecond

func (h *broadcastHost) holds(what string, condition func() bool) {
	h.t.Helper()
	window := time.NewTimer(endedRunWindow)
	defer window.Stop()
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		if !condition() {
			h.t.Fatalf("%s did not hold", what)
		}
		select {
		case <-tick.C:
			h.clock.Advance(hostStep)
		case <-window.C:
			return
		}
	}
}

func newEndedItemRun(t *testing.T) (*broadcastHost, durable.RunRef) {
	t.Helper()
	host := newBroadcastHost(t, &hostSetup{policy: testRunPolicy()})
	host.store.breakLoads(1)
	ref := host.startRun(scheduledNow(host))
	host.awaitDurableState(ref, durable.RunFailed)
	if state, _, _ := host.store.stateOf(ref.Key); state != StateScheduled {
		t.Fatalf("item after the failed run = %s, want %s", state, StateScheduled)
	}
	return host, ref
}

func TestResumeActiveRestartsAnEndedRun(t *testing.T) {
	t.Run("the resume delivers the item", func(t *testing.T) {
		host, ref := newEndedItemRun(t)
		resumed, err := ResumeActive(t.Context(), host.runtime, &fakeResumeStore{items: []FullItem{{Item: Item{ID: ref.Key}}}})
		if err != nil || resumed != 1 {
			t.Fatalf("ResumeActive() = %d, %v, want 1 resumed", resumed, err)
		}
		if state := host.durableState(ref); state == durable.RunFailed {
			t.Fatalf("durable run after the resume = %s, want a new run", state)
		}
		host.awaitItem(ref.Key, StateSucceeded)
		host.awaitDurableState(ref, durable.RunSucceeded)
		if sends := len(host.primary.sent()); sends != 1 {
			t.Errorf("primary sends = %d, want 1", sends)
		}
		if loads := host.store.count("Load"); loads < 2 {
			t.Errorf("loads = %d, want the failed one and the resumed run's", loads)
		}
	})
	t.Run("a start leaves the ended run", func(t *testing.T) {
		host, ref := newEndedItemRun(t)
		loads := host.store.count("Load")
		if err := StartItemRun(t.Context(), host.runtime, ref.Key); err != nil {
			t.Fatalf("StartItemRun(): %v", err)
		}
		host.holds("the ended run without a new invocation", func() bool {
			return host.durableState(ref) == durable.RunFailed && host.store.count("Load") == loads
		})
		if state, _, _ := host.store.stateOf(ref.Key); state != StateScheduled {
			t.Errorf("item = %s, want %s", state, StateScheduled)
		}
		if sends := len(host.primary.sent()); sends != 0 {
			t.Errorf("primary sends = %d, want 0", sends)
		}
	})
}
