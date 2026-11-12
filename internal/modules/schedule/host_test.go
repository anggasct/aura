package schedule

import (
	"context"
	"database/sql"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/durable/sqlite"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/store/storetest"
)

const (
	hostPoll    = 10 * time.Millisecond
	hostTTL     = time.Second
	hostGrace   = 5 * time.Second
	hostTimeout = 20 * time.Second
	hostTick    = 2 * time.Millisecond
	hostSteady  = 200 * time.Millisecond
)

func hourly(spec *JobSpec) {
	spec.CronExpression = "0 * * * *"
}

type invocationLog struct {
	mu   sync.Mutex
	keys []string
}

func (l *invocationLog) add(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.keys = append(l.keys, key)
}

func (l *invocationLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.keys = nil
}

func (l *invocationLog) invoked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Contains(l.keys, key)
}

type scheduleHost struct {
	t       *testing.T
	db      *sql.DB
	clock   *durable.ManualClock
	store   *fakeJobStore
	turns   *fakeTurnRunner
	notify  *fakeNotifier
	job     Job
	log     *invocationLog
	runtime *sqlite.Runtime
	stop    context.CancelFunc
	stopped chan error
}

func newScheduleHost(t *testing.T, mutate func(*JobSpec)) *scheduleHost {
	t.Helper()
	spec := defaultRunSpec()
	if mutate != nil {
		mutate(spec)
	}
	h := &scheduleHost{
		t: t, db: storetest.OpenMigrated(t), clock: durable.NewManualClock(time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)),
		store: newFakeJobStore(), turns: &fakeTurnRunner{}, notify: &fakeNotifier{}, log: &invocationLog{},
	}
	service, err := NewService(h.store)
	if err != nil {
		t.Fatalf("NewService(): %v", err)
	}
	h.job, err = service.Create(t.Context(), spec)
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}
	h.startHost()
	t.Cleanup(h.stopHost)
	return h
}

func (h *scheduleHost) prepareHost() {
	h.t.Helper()
	rt, err := sqlite.New(&sqlite.Options{DB: h.db, Logger: slog.New(slog.DiscardHandler), Clock: h.clock, PollInterval: hostPoll, HostTTL: hostTTL, StopGrace: hostGrace})
	if err != nil {
		h.t.Fatalf("sqlite.New: %v", err)
	}
	runner, err := NewRunner(&RunnerDeps{Jobs: h.store, Turns: h.turns, Notify: h.notify, Starter: rt, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		h.t.Fatalf("NewRunner(): %v", err)
	}
	runner.now = h.clock.Now
	rt.RegisterHandler(HandlerName, func(ctx context.Context, inv durable.Invocation) error {
		h.log.add(inv.Run().Key)
		return runner.Handle(ctx, inv)
	})
	h.runtime = rt
}

func (h *scheduleHost) runHost() {
	ctx, stop := context.WithCancel(context.WithoutCancel(h.t.Context()))
	stopped := make(chan error, 1)
	rt := h.runtime
	go func() { stopped <- rt.Run(ctx) }()
	h.stop, h.stopped = stop, stopped
}

func (h *scheduleHost) startHost() {
	h.t.Helper()
	h.prepareHost()
	h.runHost()
}

func (h *scheduleHost) stopHost() {
	h.t.Helper()
	if h.stop == nil {
		return
	}
	h.stop()
	h.stop = nil
	select {
	case err := <-h.stopped:
		if err != nil {
			h.t.Errorf("stop the durable host: %v", err)
		}
	case <-time.After(hostTimeout):
		h.t.Error("the durable host did not stop")
	}
}

func (h *scheduleHost) startFire(instant time.Time) string {
	h.t.Helper()
	if err := StartFire(h.t.Context(), &FireRequest{
		Starter: h.runtime, JobID: h.job.ID, Instant: instant, Chain: true,
		Expression: h.job.CronExpression, Timezone: h.job.Timezone,
	}); err != nil {
		h.t.Fatalf("StartFire(): %v", err)
	}
	return fireKey(h.job.ID, instant)
}

const runsByKey = `SELECT COUNT(*) FROM durable_run WHERE key = ?`

func (h *scheduleHost) count(ctx context.Context, query, key string) int {
	h.t.Helper()
	var n int
	if err := h.db.QueryRowContext(ctx, query, key).Scan(&n); err != nil {
		h.t.Fatalf("count %s: %v", key, err)
	}
	return n
}

func (h *scheduleHost) timers(key string) int {
	return h.count(h.t.Context(), `SELECT COUNT(*) FROM durable_timer WHERE run_key = ?`, key)
}

func (h *scheduleHost) actions(key string) int {
	return h.count(h.t.Context(), `SELECT COUNT(*) FROM durable_action WHERE run_key = ?`, key)
}

func (h *scheduleHost) runs(handler string) int {
	return h.count(h.t.Context(), `SELECT COUNT(*) FROM durable_run WHERE handler = ?`, handler)
}

func (h *scheduleHost) exists(key string) bool {
	return h.count(h.t.Context(), runsByKey, key) > 0
}

func (h *scheduleHost) state(key string) durable.RunState {
	h.t.Helper()
	status, err := h.runtime.Status(h.t.Context(), durable.RunRef{Key: key})
	if err != nil {
		h.t.Fatalf("status of %s: %v", key, err)
	}
	return status.State
}

func (h *scheduleHost) await(what string, reached func() bool) {
	h.t.Helper()
	deadline := time.NewTimer(hostTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(hostTick)
	defer tick.Stop()
	for !reached() {
		select {
		case <-tick.C:
		case <-deadline.C:
			h.t.Fatalf("timed out waiting until %s", what)
		}
	}
}

func (h *scheduleHost) steady(what string, holds func() bool) {
	h.t.Helper()
	window := time.NewTimer(hostSteady)
	defer window.Stop()
	tick := time.NewTicker(hostTick)
	defer tick.Stop()
	for {
		if !holds() {
			h.t.Fatalf("%s did not hold", what)
		}
		select {
		case <-tick.C:
		case <-window.C:
			return
		}
	}
}

func (h *scheduleHost) awaitSleeping(key string, timers int) {
	h.t.Helper()
	h.await(key+" records its timers", func() bool { return h.timers(key) >= timers })
}

func (h *scheduleHost) awaitState(key string, state durable.RunState) {
	h.t.Helper()
	h.await(key+" reaches "+string(state), func() bool { return h.state(key) == state })
}

func awaitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(hostTimeout):
		t.Fatalf("timed out waiting until %s", what)
	}
}

type hostObservation struct {
	mu                sync.Mutex
	successorExisted  bool
	stateAtNotify     string
	stateAtNotifySeen bool
}

func TestOccurrenceRunHandlesItsFireAndStartsTheNext(t *testing.T) {
	cases := []struct {
		name     string
		result   TurnResult
		state    string
		priority string
	}{
		{name: "a completed turn", result: TurnResult{TurnID: "turn-1", Text: "done", Succeeded: true}, state: OccurrenceCompleted, priority: PriorityInfo},
		{name: "a failed turn", result: TurnResult{TurnID: "turn-1", Text: "broke", SafeErrorCode: "model_failed"}, state: OccurrenceFailed, priority: PriorityWarning},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newScheduleHost(t, func(spec *JobSpec) { spec.CronExpression = "*/10 * * * *" })
			fire := h.clock.Now().Add(20 * time.Minute)
			nextKey := fireKey(h.job.ID, fire.Add(10*time.Minute))
			seen := &hostObservation{}
			h.turns.setRespond(func(ctx context.Context, _ int) (TurnResult, error) {
				seen.mu.Lock()
				defer seen.mu.Unlock()
				seen.successorExisted = h.count(ctx, runsByKey, nextKey) > 0
				return tc.result, nil
			})
			h.notify.setRespond(func(context.Context, int) error {
				seen.mu.Lock()
				defer seen.mu.Unlock()
				seen.stateAtNotify, seen.stateAtNotifySeen = h.store.stateAt(h.job.ID, fire), true
				return nil
			})
			key := h.startFire(fire)
			h.awaitSleeping(key, 1)
			h.clock.Advance(21 * time.Minute)
			h.awaitState(key, durable.RunSucceeded)

			requests := h.turns.requests()
			if len(requests) != 1 {
				t.Fatalf("turn requests = %d, want 1", len(requests))
			}
			want := TurnRequest{SessionID: "cron:" + h.job.ID, PrincipalID: principal.OwnerID, Origin: "internal", Prompt: "summarize", IdempotencyKey: turnKey(h.job.ID, fire)}
			if requests[0] != want {
				t.Errorf("turn request = %+v, want %+v", requests[0], want)
			}
			if state := h.store.stateAt(h.job.ID, fire); state != tc.state {
				t.Errorf("occurrence = %q, want %q", state, tc.state)
			}
			notices := h.notify.notices()
			if len(notices) != 1 || notices[0].Priority != tc.priority || notices[0].Text != tc.result.Text || notices[0].DestinationAlias != "default" || notices[0].Producer != "cron" {
				t.Errorf("notifications = %+v, want one %s notice", notices, tc.priority)
			}
			seen.mu.Lock()
			defer seen.mu.Unlock()
			if seen.successorExisted {
				t.Error("the next fire was started before the turn returned")
			}
			if !seen.stateAtNotifySeen || seen.stateAtNotify != OccurrenceFired {
				t.Errorf("occurrence at the notification = %q, want %q", seen.stateAtNotify, OccurrenceFired)
			}
			if state := h.state(nextKey); state != durable.RunRunning {
				t.Errorf("next fire run = %s, want running", state)
			}
		})
	}
}

func TestOccurrenceRunSleepsOnOneTimer(t *testing.T) {
	h := newScheduleHost(t, hourly)
	fire := h.clock.Now().Add(3 * time.Hour)
	key := h.startFire(fire)
	h.awaitSleeping(key, 1)
	reads := h.store.reads()
	for range 12 {
		h.clock.Advance(5 * time.Minute)
	}
	h.steady("one timer and no action while the run sleeps", func() bool {
		return h.timers(key) == 1 && h.actions(key) == 0 && h.state(key) == durable.RunRunning
	})
	if got := h.store.reads(); got != reads {
		t.Errorf("job reads while sleeping = %d, want %d", got, reads)
	}
	if h.exists(h.job.ID) {
		t.Error("a run is keyed by the bare job id")
	}

	manual := h.clock.Now().Add(-time.Minute).Truncate(time.Second).Add(11 * time.Nanosecond)
	h.store.seed(&Occurrence{ID: "cron-manual", JobID: h.job.ID, ScheduledForUTC: manual, State: OccurrenceFired, CreatedAt: h.clock.Now(), UpdatedAt: h.clock.Now()})
	if err := StartFire(t.Context(), &FireRequest{Starter: h.runtime, JobID: h.job.ID, Instant: manual}); err != nil {
		t.Fatalf("StartFire(): %v", err)
	}
	recorded := fireKey(h.job.ID, manual)
	h.awaitState(recorded, durable.RunSucceeded)
	if timers := h.timers(recorded); timers != 1 {
		t.Errorf("timers of the recorded run = %d, want 1", timers)
	}
}

func TestOccurrenceRunResumesItsSleepAfterTheHostStops(t *testing.T) {
	h := newScheduleHost(t, hourly)
	fire := h.clock.Now().Add(time.Hour)
	key := h.startFire(fire)
	h.awaitSleeping(key, 1)
	h.stopHost()

	h.startHost()
	h.clock.Advance(time.Hour)
	h.awaitState(key, durable.RunSucceeded)
	if calls := h.turns.callCount(); calls != 1 {
		t.Errorf("turn submissions = %d, want 1", calls)
	}
	if state := h.store.stateAt(h.job.ID, fire); state != OccurrenceCompleted {
		t.Errorf("occurrence = %q, want completed", state)
	}
}

func blocksOnceUntilItsContextEnds(entered chan struct{}) func(ctx context.Context, call int) (TurnResult, error) {
	return func(ctx context.Context, call int) (TurnResult, error) {
		if call > 1 {
			return TurnResult{TurnID: "turn-2", Text: "done", Succeeded: true}, nil
		}
		close(entered)
		<-ctx.Done()
		return TurnResult{TurnID: "turn-1", SafeErrorCode: "turn_failed"}, nil
	}
}

func (h *scheduleHost) preemptTheTurn() (string, time.Time) {
	h.t.Helper()
	entered := make(chan struct{})
	h.turns.setRespond(blocksOnceUntilItsContextEnds(entered))
	fire := h.clock.Now().Add(time.Hour)
	key := h.startFire(fire)
	h.awaitSleeping(key, 1)
	h.clock.Advance(time.Hour)
	awaitSignal(h.t, entered, "the turn runs")
	h.stopHost()
	if state := h.store.stateAt(h.job.ID, fire); state != OccurrenceFired {
		h.t.Errorf("occurrence after the stop = %q, want fired", state)
	}
	if count := settles(h.store.recordedWrites()); count != 0 {
		h.t.Errorf("settles after the stop = %d, want 0", count)
	}
	if notices := h.notify.notices(); len(notices) != 0 {
		h.t.Errorf("notifications after the stop = %+v, want none", notices)
	}
	if runs := h.runs(HandlerName); runs != 1 {
		h.t.Errorf("schedule runs after the stop = %d, want 1 (no successor)", runs)
	}
	if state := h.state(key); state != durable.RunRunning {
		h.t.Errorf("durable run after the stop = %s, want running", state)
	}
	return key, fire
}

func TestOccurrenceRunLeavesAFireItsHostPreemptedUnsettled(t *testing.T) {
	t.Run("a new host submits the turn again", func(t *testing.T) {
		h := newScheduleHost(t, hourly)
		key, fire := h.preemptTheTurn()

		h.startHost()
		h.awaitState(key, durable.RunSucceeded)
		requests := h.turns.requests()
		if len(requests) != 2 || requests[0].IdempotencyKey != requests[1].IdempotencyKey {
			t.Errorf("turn requests = %+v, want two with one key", requests)
		}
		if state := h.store.stateAt(h.job.ID, fire); state != OccurrenceCompleted {
			t.Errorf("occurrence = %q, want completed", state)
		}
		if notices := h.notify.notices(); len(notices) != 1 {
			t.Errorf("notifications = %d, want 1", len(notices))
		}
	})
	t.Run("a new host after the grace expires the fire", func(t *testing.T) {
		h := newScheduleHost(t, hourly)
		key, fire := h.preemptTheTurn()

		h.clock.Advance(time.Duration(h.job.CatchUpGraceSeconds)*time.Second + time.Minute)
		h.startHost()
		h.awaitState(key, durable.RunSucceeded)
		if calls := h.turns.callCount(); calls != 1 {
			t.Errorf("turn submissions = %d, want 1", calls)
		}
		if state := h.store.stateAt(h.job.ID, fire); state != OccurrenceExpired {
			t.Errorf("occurrence = %q, want expired", state)
		}
	})
}

func blocksOnceUntilTheContextEnds(entered chan struct{}) func(ctx context.Context) error {
	var once sync.Once
	return func(ctx context.Context) error {
		first := false
		once.Do(func() { first = true })
		if !first {
			return nil
		}
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
}

func TestOccurrenceRunSettlesAfterAStopAroundTheNotification(t *testing.T) {
	cases := []struct {
		name    string
		block   func(h *scheduleHost, entered chan struct{})
		notices int
	}{
		{name: "a stop inside the notification", notices: 2, block: func(h *scheduleHost, entered chan struct{}) {
			blocks := blocksOnceUntilTheContextEnds(entered)
			h.notify.setRespond(func(ctx context.Context, _ int) error { return blocks(ctx) })
		}},
		{name: "a stop inside the settle", notices: 1, block: func(h *scheduleHost, entered chan struct{}) {
			h.store.setSettleHook(blocksOnceUntilTheContextEnds(entered))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newScheduleHost(t, hourly)
			entered := make(chan struct{})
			tc.block(h, entered)
			fire := h.clock.Now().Add(time.Hour)
			key := h.startFire(fire)
			h.awaitSleeping(key, 1)
			h.clock.Advance(time.Hour)
			awaitSignal(t, entered, "the run blocks")
			h.stopHost()
			if state := h.store.stateAt(h.job.ID, fire); state != OccurrenceFired {
				t.Errorf("occurrence after the stop = %q, want fired", state)
			}

			h.startHost()
			h.awaitState(key, durable.RunSucceeded)
			if calls := h.turns.callCount(); calls != 1 {
				t.Errorf("turn submissions = %d, want 1", calls)
			}
			if notices := h.notify.notices(); len(notices) != tc.notices {
				t.Errorf("notifier calls = %d, want %d", len(notices), tc.notices)
			}
			if state := h.store.stateAt(h.job.ID, fire); state != OccurrenceCompleted {
				t.Errorf("occurrence = %q, want completed", state)
			}
		})
	}
}

func TestOccurrenceRunIsNotInvokedAgainOnceItEnded(t *testing.T) {
	h := newScheduleHost(t, func(spec *JobSpec) { spec.CronExpression = "*/10 * * * *" })
	first := h.clock.Now().Add(10 * time.Minute)
	fires := []time.Time{first, first.Add(10 * time.Minute)}
	h.startFire(first)
	for _, fire := range fires {
		key := fireKey(h.job.ID, fire)
		h.awaitSleeping(key, 1)
		h.clock.Advance(10 * time.Minute)
		h.awaitState(key, durable.RunSucceeded)
	}
	third := fireKey(h.job.ID, first.Add(20*time.Minute))
	h.awaitSleeping(third, 1)
	for _, fire := range fires {
		h.store.remove(h.job.ID, fire)
	}
	h.stopHost()

	h.prepareHost()
	h.log.reset()
	if _, err := StartRuns(t.Context(), &RunsRequest{Starter: h.runtime, Store: h.store, Now: h.clock.Now()}); err != nil {
		t.Fatalf("StartRuns(): %v", err)
	}
	for _, fire := range fires {
		h.startFire(fire)
	}
	h.runHost()
	h.await("the sleeping run is invoked again", func() bool { return h.log.invoked(third) })
	for _, fire := range fires {
		key := fireKey(h.job.ID, fire)
		if h.log.invoked(key) {
			t.Errorf("ended run %s was invoked again", key)
		}
		if state := h.state(key); state != durable.RunSucceeded {
			t.Errorf("ended run %s = %s, want succeeded", key, state)
		}
	}
	if calls := h.turns.callCount(); calls != 2 {
		t.Errorf("turn submissions = %d, want 2", calls)
	}
}

func TestOccurrenceRunHandlesEachMissedFire(t *testing.T) {
	h := newScheduleHost(t, func(spec *JobSpec) {
		spec.CronExpression, spec.CatchUpGraceSeconds = "*/10 * * * *", 60
	})
	first := h.clock.Now().Add(10 * time.Minute)
	key := h.startFire(first)
	h.awaitSleeping(key, 1)
	h.clock.Advance(35 * time.Minute)
	pending := fireKey(h.job.ID, first.Add(30*time.Minute))
	h.await("the first fire that is not due sleeps", func() bool { return h.exists(pending) && h.timers(pending) == 1 })
	for i := range 3 {
		fire := first.Add(time.Duration(i) * 10 * time.Minute)
		if state := h.store.stateAt(h.job.ID, fire); state != OccurrenceExpired {
			t.Errorf("occurrence %v = %q, want expired", fire, state)
		}
	}
	if calls := h.turns.callCount(); calls != 0 {
		t.Errorf("turn submissions = %d, want 0", calls)
	}
	if state := h.state(pending); state != durable.RunRunning {
		t.Errorf("run of the fire that is not due = %s, want running", state)
	}
}

func TestOverloadOutcomeSurvivesAReplay(t *testing.T) {
	h := newScheduleHost(t, hourly)
	h.turns.scripts = []turnScript{
		{err: Errorf(ErrorCodeRuntimeOverloaded, "pending turn queue is full")},
		{result: TurnResult{TurnID: "turn-2", Text: "recovered", Succeeded: true}},
	}
	fire := h.clock.Now().Add(time.Hour)
	key := h.startFire(fire)
	h.awaitSleeping(key, 1)
	h.clock.Advance(time.Hour)
	h.awaitSleeping(key, 2)
	h.stopHost()

	h.startHost()
	h.clock.Advance(overlapPollInterval + time.Second)
	h.awaitState(key, durable.RunSucceeded)
	if calls := h.turns.callCount(); calls != 2 {
		t.Errorf("turn submissions = %d, want 2", calls)
	}
	if state := h.store.stateAt(h.job.ID, fire); state != OccurrenceCompleted {
		t.Errorf("occurrence = %q, want completed", state)
	}
}

func TestStartFireStartsOneRunPerInstant(t *testing.T) {
	h := newScheduleHost(t, hourly)
	fire := h.clock.Now().Add(24 * time.Hour)
	h.startFire(fire)
	h.startFire(fire)
	if runs := h.runs(HandlerName); runs != 1 {
		t.Errorf("runs after two starts of one instant = %d, want 1", runs)
	}
	h.startFire(fire.Add(time.Minute))
	if runs := h.runs(HandlerName); runs != 2 {
		t.Errorf("runs after a second instant = %d, want 2", runs)
	}
}
