package server

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

type lifecycleRecord struct {
	mu     sync.Mutex
	events []string
}

func (r *lifecycleRecord) add(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *lifecycleRecord) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

type recordedLifecycle struct {
	name     string
	record   *lifecycleRecord
	startErr error
	stopErr  error
	onStop   func(ctx context.Context)
}

var _ Lifecycle = (*recordedLifecycle)(nil)

func (l *recordedLifecycle) Name() string { return l.name }

func (l *recordedLifecycle) Start(context.Context) error {
	l.record.add("start " + l.name)
	return l.startErr
}

func (l *recordedLifecycle) Stop(ctx context.Context) error {
	l.record.add("stop " + l.name)
	if l.onStop != nil {
		l.onStop(ctx)
	}
	return l.stopErr
}

func recordedRelease(record *lifecycleRecord, name string, err error) func(context.Context) error {
	return func(context.Context) error {
		record.add("stop " + name)
		return err
	}
}

func newTestLifecycleGroup() *LifecycleGroup {
	return NewLifecycleGroup(LifecycleOptions{StopTimeout: time.Minute, Logger: discardLogger()})
}

func mustAdd(t *testing.T, group *LifecycleGroup, entry Lifecycle) {
	t.Helper()
	if err := group.Add(entry); err != nil {
		t.Fatalf("Add(%s): %v", entry.Name(), err)
	}
}

func mustDefer(t *testing.T, group *LifecycleGroup, name string, stop func(context.Context) error) {
	t.Helper()
	if err := group.Defer(name, stop); err != nil {
		t.Fatalf("Defer(%s): %v", name, err)
	}
}

func assertLifecycleEvents(t *testing.T, record *lifecycleRecord, want ...string) {
	t.Helper()
	if got := record.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("lifecycle events = %v, want %v", got, want)
	}
}

func TestLifecycleGroupStartsInOrderAndStopsInReverse(t *testing.T) {
	record := &lifecycleRecord{}
	group := newTestLifecycleGroup()
	mustDefer(t, group, "database", recordedRelease(record, "database", nil))
	mustAdd(t, group, &recordedLifecycle{name: "purge", record: record})
	mustDefer(t, group, "index", recordedRelease(record, "index", nil))
	mustAdd(t, group, &recordedLifecycle{name: "channels", record: record})

	if err := group.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	assertLifecycleEvents(t, record, "start purge", "start channels")
	if err := group.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	assertLifecycleEvents(t, record,
		"start purge", "start channels",
		"stop channels", "stop index", "stop purge", "stop database")
}

func TestLifecycleGroupNamesFollowRegistrationOrder(t *testing.T) {
	record := &lifecycleRecord{}
	group := newTestLifecycleGroup()
	mustDefer(t, group, "database", recordedRelease(record, "database", nil))
	mustAdd(t, group, &recordedLifecycle{name: "purge", record: record})
	mustDefer(t, group, "index", recordedRelease(record, "index", nil))
	mustAdd(t, group, Background("sweeps", func(context.Context) {}))

	want := []string{"database", "purge", "index", "sweeps"}
	if got := group.Names(); !slices.Equal(got, want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	if got := newTestLifecycleGroup().Names(); len(got) != 0 {
		t.Fatalf("Names() of an empty group = %v, want none", got)
	}
}

func TestLifecycleGroupStopsOnlyWhatStarted(t *testing.T) {
	t.Run("a failed start stops the entries before it", func(t *testing.T) {
		record := &lifecycleRecord{}
		group := newTestLifecycleGroup()
		startFailure := errors.New("third entry refuses to start")
		mustDefer(t, group, "database", recordedRelease(record, "database", nil))
		mustAdd(t, group, &recordedLifecycle{name: "first", record: record})
		mustAdd(t, group, &recordedLifecycle{name: "second", record: record})
		mustAdd(t, group, &recordedLifecycle{name: "third", record: record, startErr: startFailure})
		mustAdd(t, group, &recordedLifecycle{name: "fourth", record: record})

		if err := group.Start(t.Context()); !errors.Is(err, startFailure) {
			t.Fatalf("Start error = %v, want the third entry's failure", err)
		}
		if err := group.Stop(t.Context()); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		assertLifecycleEvents(t, record,
			"start first", "start second", "start third",
			"stop second", "stop first", "stop database")
	})

	t.Run("a group that never started releases only deferred entries", func(t *testing.T) {
		record := &lifecycleRecord{}
		group := newTestLifecycleGroup()
		mustDefer(t, group, "database", recordedRelease(record, "database", nil))
		mustAdd(t, group, &recordedLifecycle{name: "purge", record: record})

		if err := group.Stop(t.Context()); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		assertLifecycleEvents(t, record, "stop database")
	})
}

func TestLifecycleGroupStopRunsEveryEntryAndJoinsErrors(t *testing.T) {
	record := &lifecycleRecord{}
	group := newTestLifecycleGroup()
	databaseFailure := errors.New("database close failed")
	channelFailure := errors.New("channels did not drain")
	mustDefer(t, group, "database", recordedRelease(record, "database", databaseFailure))
	mustAdd(t, group, &recordedLifecycle{name: "purge", record: record})
	mustAdd(t, group, &recordedLifecycle{name: "channels", record: record, stopErr: channelFailure})

	if err := group.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	err := group.Stop(t.Context())
	if !errors.Is(err, databaseFailure) || !errors.Is(err, channelFailure) {
		t.Fatalf("Stop error = %v, want both failures", err)
	}
	assertLifecycleEvents(t, record,
		"start purge", "start channels",
		"stop channels", "stop purge", "stop database")
}

func TestLifecycleGroupStopIsIdempotent(t *testing.T) {
	record := &lifecycleRecord{}
	group := newTestLifecycleGroup()
	mustDefer(t, group, "database", recordedRelease(record, "database", errors.New("close failed once")))
	mustAdd(t, group, &recordedLifecycle{name: "purge", record: record})
	if err := group.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := group.Stop(t.Context()); err == nil {
		t.Fatal("first Stop returned nil, want the close failure")
	}
	if err := group.Stop(t.Context()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	assertLifecycleEvents(t, record, "start purge", "stop purge", "stop database")
}

func TestLifecycleGroupStopDetachesFromACancelledContext(t *testing.T) {
	record := &lifecycleRecord{}
	group := NewLifecycleGroup(LifecycleOptions{StopTimeout: time.Minute, Logger: discardLogger()})
	type observedContext struct {
		err         error
		hasDeadline bool
	}
	var observed []observedContext
	observe := func(ctx context.Context) {
		_, hasDeadline := ctx.Deadline()
		observed = append(observed, observedContext{err: ctx.Err(), hasDeadline: hasDeadline})
	}
	mustDefer(t, group, "database", func(ctx context.Context) error {
		observe(ctx)
		return nil
	})
	mustAdd(t, group, &recordedLifecycle{name: "channels", record: record, onStop: observe})
	if err := group.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := group.Stop(cancelled); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(observed) != 2 {
		t.Fatalf("observed %d stop contexts, want 2", len(observed))
	}
	for i, got := range observed {
		if got.err != nil || !got.hasDeadline {
			t.Errorf("stop context %d: err = %v, has deadline = %v; want a live context with a deadline", i, got.err, got.hasDeadline)
		}
	}
}

func TestLifecycleGroupRejectsInvalidUse(t *testing.T) {
	noop := func(context.Context) error { return nil }
	cases := []struct {
		name string
		use  func(t *testing.T, group *LifecycleGroup) error
	}{
		{"nil entry", func(_ *testing.T, group *LifecycleGroup) error { return group.Add(nil) }},
		{"entry without a name", func(_ *testing.T, group *LifecycleGroup) error {
			return group.Add(&recordedLifecycle{record: &lifecycleRecord{}})
		}},
		{"deferred release without a name", func(_ *testing.T, group *LifecycleGroup) error { return group.Defer("", noop) }},
		{"deferred release without a stop function", func(_ *testing.T, group *LifecycleGroup) error {
			return group.Defer("database", nil)
		}},
		{"entry name registered twice", func(t *testing.T, group *LifecycleGroup) error {
			mustDefer(t, group, "database", noop)
			return group.Add(&recordedLifecycle{name: "database", record: &lifecycleRecord{}})
		}},
		{"add after start", func(t *testing.T, group *LifecycleGroup) error {
			if err := group.Start(t.Context()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			return group.Add(&recordedLifecycle{name: "late", record: &lifecycleRecord{}})
		}},
		{"defer after stop", func(t *testing.T, group *LifecycleGroup) error {
			if err := group.Stop(t.Context()); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			return group.Defer("late", noop)
		}},
		{"second start", func(t *testing.T, group *LifecycleGroup) error {
			if err := group.Start(t.Context()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			return group.Start(t.Context())
		}},
		{"start after stop", func(t *testing.T, group *LifecycleGroup) error {
			if err := group.Stop(t.Context()); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			return group.Start(t.Context())
		}},
		{"background without a run function", func(t *testing.T, group *LifecycleGroup) error {
			mustAdd(t, group, Background("loop", nil))
			return group.Start(t.Context())
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.use(t, newTestLifecycleGroup())
			if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
				t.Fatalf("error = %v, want %s", err, ErrorCodeInvalidArgument)
			}
		})
	}
}

func TestBackgroundStopWaitsForRunToReturn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var unwound atomic.Bool
		loop := Background("purge", func(ctx context.Context) {
			<-ctx.Done()
			time.Sleep(5 * time.Second)
			unwound.Store(true)
		})
		if err := loop.Start(t.Context()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		synctest.Wait()

		stopCtx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		if err := loop.Stop(stopCtx); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if !unwound.Load() {
			t.Fatal("Stop returned before the run function had returned")
		}
	})
}

func TestBackgroundStopTimesOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		loop := Background("purge", func(context.Context) { <-release })
		if err := loop.Start(t.Context()); err != nil {
			t.Fatalf("Start: %v", err)
		}

		stopCtx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		err := loop.Stop(stopCtx)
		if code, ok := CodeOf(err); !ok || code != ErrorCodeStopTimeout {
			t.Fatalf("Stop error = %v, want %s", err, ErrorCodeStopTimeout)
		}
		close(release)
		synctest.Wait()
	})
}

func TestBackgroundStopWithoutStartIsANoOp(t *testing.T) {
	loop := Background("purge", func(context.Context) {})
	if err := loop.Stop(t.Context()); err != nil {
		t.Fatalf("Stop without Start: %v", err)
	}
}
