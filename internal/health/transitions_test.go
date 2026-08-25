package health

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

type captureSink struct {
	transitions []Transition
	failNext    int
}

func (c *captureSink) save(_ context.Context, t *Transition) error {
	if c.failNext > 0 {
		c.failNext--
		return errors.New("sink unavailable")
	}
	c.transitions = append(c.transitions, *t)
	return nil
}

func (c *captureSink) codes() []Transition {
	return append([]Transition(nil), c.transitions...)
}

func baseTime() time.Time { return time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC) }

func findingAt(id string, status Status) Finding {
	return Finding{ID: id, Component: "backup", Code: "backup_stale", Status: status}
}

// First sighting transitions from none so the durable log carries complete
// history; repeat observations of an unchanged state emit nothing.
func TestTrackerEmitsInitialAndSuppressesUnchanged(t *testing.T) {
	sink := &captureSink{}
	now := baseTime()
	tracker, err := NewStateTracker(TransitionPolicy{StableFor: time.Minute, Cooldown: time.Minute}, sink.save, nil)
	if err != nil {
		t.Fatalf("NewStateTracker: %v", err)
	}
	tracker.SetClock(func() time.Time { return now })

	if _, err := tracker.Observe(context.Background(), []Finding{findingAt("backup/ok", StatusUp)}); err != nil {
		t.Fatalf("observe: %v", err)
	}
	emitted := sink.codes()
	if len(emitted) != 1 || emitted[0].From != StatusNone || emitted[0].To != StatusUp {
		t.Fatalf("initial emissions = %+v", emitted)
	}

	for range 5 {
		now = now.Add(time.Minute)
		if _, err := tracker.Observe(context.Background(), []Finding{findingAt("backup/ok", StatusUp)}); err != nil {
			t.Fatalf("repeated observe: %v", err)
		}
	}
	if got := len(sink.codes()); got != 1 {
		t.Errorf("emissions after unchanged observations = %d, want 1", got)
	}
}

// Hysteresis: a candidate state that reverts before StableFor commits
// produces no transition.
func TestTrackerSuppressesFlaps(t *testing.T) {
	sink := &captureSink{}
	now := baseTime()
	tracker, _ := NewStateTracker(TransitionPolicy{StableFor: time.Minute, Cooldown: time.Minute}, sink.save, nil)
	tracker.SetClock(func() time.Time { return now })
	ctx := context.Background()

	if _, err := tracker.Observe(ctx, []Finding{findingAt("f", StatusUp)}); err != nil {
		t.Fatal(err)
	}
	for _, status := range []Status{StatusDegraded, StatusUp, StatusDown, StatusUp} {
		now = now.Add(10 * time.Second)
		if _, err := tracker.Observe(ctx, []Finding{findingAt("f", status)}); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(sink.codes()); got != 1 {
		t.Fatalf("flap emissions = %d (%+v), want only the initial", got, sink.codes())
	}

	// A state held past StableFor does commit exactly once.
	now = now.Add(time.Minute)
	if _, err := tracker.Observe(ctx, []Finding{findingAt("f", StatusDown)}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := tracker.Observe(ctx, []Finding{findingAt("f", StatusDown)}); err != nil {
		t.Fatal(err)
	}
	emitted := sink.codes()
	if len(emitted) != 2 || emitted[1].To != StatusDown || emitted[1].From != StatusUp {
		t.Fatalf("committed transitions = %+v", emitted)
	}
}

// Cooldown defers the second committed transition until the configured
// interval passes, even when the debounce window already elapsed.
func TestTrackerCooldownDefersCommit(t *testing.T) {
	sink := &captureSink{}
	now := baseTime()
	tracker, _ := NewStateTracker(TransitionPolicy{StableFor: time.Minute, Cooldown: 10 * time.Minute}, sink.save, nil)
	tracker.SetClock(func() time.Time { return now })
	ctx := context.Background()

	if _, err := tracker.Observe(ctx, []Finding{findingAt("f", StatusUp)}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := tracker.Observe(ctx, []Finding{findingAt("f", StatusDegraded)}); err != nil {
		t.Fatal(err)
	}
	if got := len(sink.codes()); got != 1 {
		t.Fatalf("transitions before cooldown = %+v, want only initial", sink.codes())
	}
	now = now.Add(9 * time.Minute)
	if _, err := tracker.Observe(ctx, []Finding{findingAt("f", StatusDegraded)}); err != nil {
		t.Fatal(err)
	}
	emitted := sink.codes()
	if len(emitted) != 2 || emitted[1].To != StatusDegraded {
		t.Fatalf("post-cooldown transitions = %+v", emitted)
	}
}

// A failed sink must not advance tracked state: the same transition is
// retried on the next observation and emitted exactly once once persistence
// recovers.
func TestTrackerRetriesFailedPersistence(t *testing.T) {
	sink := &captureSink{}
	now := baseTime()
	tracker, _ := NewStateTracker(TransitionPolicy{StableFor: time.Minute, Cooldown: time.Minute}, sink.save, nil)
	tracker.SetClock(func() time.Time { return now })
	ctx := context.Background()

	if _, err := tracker.Observe(ctx, []Finding{findingAt("f", StatusUp)}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := tracker.Observe(ctx, []Finding{findingAt("f", StatusDegraded)}); err != nil {
		t.Fatal(err)
	}
	sink.failNext = 1
	now = now.Add(2 * time.Minute)
	if _, err := tracker.Observe(ctx, []Finding{findingAt("f", StatusDegraded)}); err == nil {
		t.Fatal("expected sink error to surface")
	}
	snapshot := tracker.Snapshot()
	if f := findSnapshot(snapshot, "f"); f != nil && f.Status != StatusUp {
		t.Errorf("tracked status advanced past failed write: %+v", f)
	}
	now = now.Add(2 * time.Minute)
	if _, err := tracker.Observe(ctx, []Finding{findingAt("f", StatusDegraded)}); err != nil {
		t.Fatalf("retry observe: %v", err)
	}
	emitted := sink.codes()
	if len(emitted) != 2 || emitted[1].To != StatusDegraded {
		t.Fatalf("after recovery transitions = %+v", emitted)
	}
}

// Restore-from-history: a restarted tracker resumes from the last persisted
// states, emits nothing for unchanged findings, and compares new changes
// against the restored baseline.
func TestTrackerRestoresFromHistory(t *testing.T) {
	history := []Transition{
		{FindingID: "f", From: StatusNone, To: StatusUp, At: baseTime()},
		{FindingID: "f", From: StatusUp, To: StatusDegraded, At: baseTime().Add(time.Hour)},
	}
	sink := &captureSink{}
	now := baseTime().Add(2 * time.Hour)
	tracker, err := NewStateTracker(TransitionPolicy{StableFor: time.Minute, Cooldown: time.Minute}, sink.save,
		func(context.Context) ([]Transition, error) { return history, nil })
	if err != nil {
		t.Fatalf("NewStateTracker: %v", err)
	}
	tracker.SetClock(func() time.Time { return now })

	if _, err := tracker.Observe(context.Background(), []Finding{findingAt("f", StatusDegraded)}); err != nil {
		t.Fatal(err)
	}
	if got := len(sink.codes()); got != 0 {
		t.Fatalf("restored tracker re-emitted %+v, want silence for unchanged state", sink.codes())
	}
	now = now.Add(2 * time.Minute)
	if _, err := tracker.Observe(context.Background(), []Finding{findingAt("f", StatusUp)}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := tracker.Observe(context.Background(), []Finding{findingAt("f", StatusUp)}); err != nil {
		t.Fatal(err)
	}
	emitted := sink.codes()
	if len(emitted) != 1 || emitted[0].From != StatusDegraded || emitted[0].To != StatusUp {
		t.Fatalf("post-restart transition = %+v, want degraded→up against restored baseline", emitted)
	}
	if !slices.IsSortedFunc(history, func(a, b Transition) int { return a.At.Compare(b.At) }) {
		t.Error("history fixture must stay sorted for readability")
	}
}

func findSnapshot(findings []Finding, id string) *Finding {
	for i := range findings {
		if findings[i].ID == id {
			return &findings[i]
		}
	}
	return nil
}
