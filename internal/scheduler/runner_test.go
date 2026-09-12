package scheduler

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/durable"
)

func (s *fakeJobStore) ActiveOccurrence(_ context.Context, jobID string) (Occurrence, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best *Occurrence
	for id := range s.occurrences {
		occurrence := s.occurrences[id]
		if occurrence.JobID != jobID || occurrence.State != OccurrenceFired {
			continue
		}
		if best == nil || occurrence.ScheduledForUTC.After(best.ScheduledForUTC) {
			next := occurrence
			best = &next
		}
	}
	if best == nil {
		return Occurrence{}, false, nil
	}
	return *best, true, nil
}

func (s *fakeJobStore) AttachTurn(_ context.Context, id, turnID string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	occurrence, ok := s.occurrences[id]
	if !ok {
		return Errorf(ErrorCodeJobNotFound, "occurrence not found")
	}
	if occurrence.State != OccurrenceFired {
		return Errorf(ErrorCodeOccurrenceConflict, "occurrence is no longer fired")
	}
	occurrence.TurnID = turnID
	s.occurrences[id] = occurrence
	return nil
}

func (s *fakeJobStore) SettleOccurrence(_ context.Context, id, state, turnID, _, _ string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	occurrence, ok := s.occurrences[id]
	if !ok {
		return Errorf(ErrorCodeJobNotFound, "occurrence not found")
	}
	if occurrence.State != OccurrenceFired {
		return Errorf(ErrorCodeOccurrenceConflict, "occurrence is already terminal")
	}
	occurrence.State = state
	if turnID != "" {
		occurrence.TurnID = turnID
	}
	s.occurrences[id] = occurrence
	return nil
}

type turnScript struct {
	result TurnResult
	err    error
}

type fakeTurnRunner struct {
	mu      sync.Mutex
	scripts []turnScript
	calls   []TurnRequest
}

func (r *fakeTurnRunner) RunTurn(_ context.Context, req *TurnRequest) (TurnResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, *req)
	if len(r.scripts) == 0 {
		return TurnResult{TurnID: "turn-1", Text: "done", Succeeded: true}, nil
	}
	script := r.scripts[0]
	r.scripts = r.scripts[1:]
	return script.result, script.err
}

func (r *fakeTurnRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

type fakeNotifier struct {
	mu    sync.Mutex
	calls []NotifyRequest
	err   error
}

func (n *fakeNotifier) Notify(_ context.Context, req *NotifyRequest) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, *req)
	return n.err
}

type scheduleFixture struct {
	runner   *Runner
	store    *fakeJobStore
	turns    *fakeTurnRunner
	notify   *fakeNotifier
	runtime  *durable.Fake
	clock    *durable.ManualClock
	job      Job
	schedule Schedule
}

func newScheduleFixture(t *testing.T, spec *JobSpec, scripts []turnScript) *scheduleFixture {
	t.Helper()
	start := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	clock := durable.NewManualClock(start)
	runtime := durable.NewFake().WithClock(clock)
	store := newFakeJobStore()
	turns := &fakeTurnRunner{scripts: scripts}
	notify := &fakeNotifier{}
	runner, err := NewRunner(store, turns, notify, nil)
	if err != nil {
		t.Fatalf("NewRunner(): %v", err)
	}
	runner.now = clock.Now
	runtime.RegisterHandler(HandlerName, runner.Handle)
	if spec == nil {
		spec = &JobSpec{
			Name: "nightly", CronExpression: "* * * * *", Timezone: "UTC",
			Prompt: "summarize", OriginChannel: "discord", OriginDestination: "default",
			OverlapPolicy: OverlapSkip, CatchUpGraceSeconds: 900,
		}
	}
	service, err := NewService(store)
	if err != nil {
		t.Fatalf("NewService(): %v", err)
	}
	job, err := service.Create(t.Context(), spec)
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}
	schedule, err := ParseExpression(job.CronExpression)
	if err != nil {
		t.Fatalf("ParseExpression(): %v", err)
	}
	return &scheduleFixture{runner: runner, store: store, turns: turns, notify: notify, runtime: runtime, clock: clock, job: job, schedule: schedule}
}

func startScheduleRun(t *testing.T, fixture *scheduleFixture, id string) {
	t.Helper()
	if _, err := fixture.runtime.Start(t.Context(), durable.StartRequest{
		Handler: HandlerName, Key: id, Payload: []byte(id),
	}); err != nil {
		t.Fatalf("Start(): %v", err)
	}
}

func waitOccurrenceState(t *testing.T, fixture *scheduleFixture, jobID, want string) Occurrence {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		fixture.clock.Advance(time.Second)
		time.Sleep(time.Millisecond)
		fixture.store.mu.Lock()
		for id := range fixture.store.occurrences {
			occurrence := fixture.store.occurrences[id]
			if occurrence.JobID == jobID && occurrence.State == want {
				found := occurrence
				fixture.store.mu.Unlock()
				return found
			}
		}
		fixture.store.mu.Unlock()
	}
	t.Fatalf("no occurrence reached %q", want)
	return Occurrence{}
}

func TestRunner_FiresTurnAndNotifies(t *testing.T) {
	fixture := newScheduleFixture(t, nil, nil)
	startScheduleRun(t, fixture, fixture.job.ID)
	completed := waitOccurrenceState(t, fixture, fixture.job.ID, OccurrenceCompleted)
	if completed.TurnID == "" {
		t.Error("completed occurrence carries no turn id")
	}
	if fixture.turns.callCount() != 1 {
		t.Fatalf("turn calls = %d, want 1", fixture.turns.callCount())
	}
	call := fixture.turns.calls[0]
	if call.SessionID != "cron:"+fixture.job.ID || call.PrincipalID != "cron" || call.Prompt != "summarize" {
		t.Errorf("turn request = %+v", call)
	}
	if call.IdempotencyKey == "" {
		t.Error("turn idempotency key missing")
	}
	fixture.notify.mu.Lock()
	defer fixture.notify.mu.Unlock()
	if len(fixture.notify.calls) != 1 {
		t.Fatalf("notifications = %d, want 1", len(fixture.notify.calls))
	}
	notice := fixture.notify.calls[0]
	if notice.Producer != "cron" || notice.Priority != PriorityInfo || notice.DestinationAlias != "default" || notice.Text != "done" {
		t.Errorf("notification = %+v", notice)
	}
}

func TestRunner_ExpiredFireSkipsTurn(t *testing.T) {
	spec := &JobSpec{
		Name: "nightly", CronExpression: "* * * * *", Timezone: "UTC",
		Prompt: "summarize", OriginChannel: "discord", OriginDestination: "default",
		OverlapPolicy: OverlapSkip, CatchUpGraceSeconds: 60,
	}
	fixture := newScheduleFixture(t, spec, nil)
	base := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	var calls atomic.Int64
	fixture.runner.now = func() time.Time {
		if calls.Add(1) == 1 {
			return base
		}
		return base.Add(24 * time.Hour)
	}
	startScheduleRun(t, fixture, fixture.job.ID)
	expired := waitOccurrenceState(t, fixture, fixture.job.ID, OccurrenceExpired)
	_ = expired
	if fixture.turns.callCount() != 0 {
		t.Error("expired fire ran a turn")
	}
	if len(fixture.notify.calls) != 0 {
		t.Error("expired fire notified")
	}
}

type stubInvocation struct{}

func (stubInvocation) Run() durable.RunRef { return durable.RunRef{} }

func (stubInvocation) Payload() []byte { return nil }

func (stubInvocation) Signal(context.Context, string) ([]byte, bool) { return nil, false }

func (stubInvocation) Sleep(time.Duration) error { return nil }

func (stubInvocation) Timer(time.Duration) <-chan time.Time { return nil }

func (stubInvocation) Wait(ctx context.Context, name string, timeout time.Duration) (payload []byte, timedOut, ok bool) {
	return nil, false, false
}

func (stubInvocation) RunAction(context.Context, string, func(context.Context) ([]byte, error)) ([]byte, error) {
	return nil, errors.New("stub invocation does not support actions")
}

func TestRunner_ExpiredFireReplayAdvances(t *testing.T) {
	spec := &JobSpec{
		Name: "nightly", CronExpression: "* * * * *", Timezone: "UTC",
		Prompt: "summarize", OriginChannel: "discord", OriginDestination: "default",
		OverlapPolicy: OverlapSkip, CatchUpGraceSeconds: 60,
	}
	fixture := newScheduleFixture(t, spec, nil)
	fire := time.Date(2026, 9, 12, 12, 1, 0, 0, time.UTC)
	now := fire.Add(time.Hour)
	inv := stubInvocation{}
	done, err := fixture.runner.fireDue(t.Context(), inv, &fixture.job, fire, now, 1)
	if err != nil {
		t.Fatalf("first expired fire: %v", err)
	}
	if !done {
		t.Fatal("first expired fire did not advance")
	}
	fresh, err := NewRunner(fixture.store, fixture.turns, fixture.notify, nil)
	if err != nil {
		t.Fatalf("NewRunner(): %v", err)
	}
	done, err = fresh.fireDue(t.Context(), inv, &fixture.job, fire, now, 1)
	if err != nil {
		t.Fatalf("expired replay: %v", err)
	}
	if !done {
		t.Fatal("expired replay did not advance")
	}
	if fixture.turns.callCount() != 0 {
		t.Error("expired replay ran a turn")
	}
	occurrence, found, err := fixture.store.OccurrenceByFire(t.Context(), fixture.job.ID, fire.UTC())
	if err != nil {
		t.Fatalf("OccurrenceByFire(): %v", err)
	}
	if !found {
		t.Fatal("expired occurrence missing after replay")
	}
	if occurrence.State != OccurrenceExpired {
		t.Errorf("occurrence state = %q, want expired", occurrence.State)
	}
}

func TestRunner_SkipOverlapWithoutTurn(t *testing.T) {
	fixture := newScheduleFixture(t, nil, nil)
	active := Occurrence{
		ID: "cron-active", JobID: fixture.job.ID, JobVersion: 1,
		ScheduledForUTC: time.Date(2026, 9, 12, 11, 0, 0, 0, time.UTC),
		State:           OccurrenceFired, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	fixture.store.mu.Lock()
	fixture.store.occurrences[active.ID] = active
	fixture.store.mu.Unlock()
	startScheduleRun(t, fixture, fixture.job.ID)
	skipped := waitOccurrenceState(t, fixture, fixture.job.ID, OccurrenceSkippedOverlap)
	_ = skipped
	if fixture.turns.callCount() != 0 {
		t.Error("skipped fire ran a turn")
	}
}

func TestRunner_SkippedFireReplayAdvances(t *testing.T) {
	fixture := newScheduleFixture(t, nil, nil)
	active := Occurrence{
		ID: "cron-active", JobID: fixture.job.ID, JobVersion: 1,
		ScheduledForUTC: time.Date(2026, 9, 12, 11, 0, 0, 0, time.UTC),
		State:           OccurrenceFired, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	fixture.store.mu.Lock()
	fixture.store.occurrences[active.ID] = active
	fixture.store.mu.Unlock()
	fire := time.Date(2026, 9, 12, 12, 1, 0, 0, time.UTC)
	now := fire.Add(time.Minute)
	inv := stubInvocation{}
	done, err := fixture.runner.fireDue(t.Context(), inv, &fixture.job, fire, now, 1)
	if err != nil {
		t.Fatalf("first skipped fire: %v", err)
	}
	if !done {
		t.Fatal("first skipped fire did not advance")
	}
	fresh, err := NewRunner(fixture.store, fixture.turns, fixture.notify, nil)
	if err != nil {
		t.Fatalf("NewRunner(): %v", err)
	}
	done, err = fresh.fireDue(t.Context(), inv, &fixture.job, fire, now, 1)
	if err != nil {
		t.Fatalf("skipped replay: %v", err)
	}
	if !done {
		t.Fatal("skipped replay did not advance")
	}
	if fixture.turns.callCount() != 0 {
		t.Error("skipped replay ran a turn")
	}
	occurrence, found, err := fixture.store.OccurrenceByFire(t.Context(), fixture.job.ID, fire.UTC())
	if err != nil {
		t.Fatalf("OccurrenceByFire(): %v", err)
	}
	if !found {
		t.Fatal("skipped occurrence missing after replay")
	}
	if occurrence.State != OccurrenceSkippedOverlap {
		t.Errorf("occurrence state = %q, want skipped_overlap", occurrence.State)
	}
}

func TestRunner_QueueOneWaitsThenPausesCleanly(t *testing.T) {
	spec := &JobSpec{
		Name: "nightly", CronExpression: "* * * * *", Timezone: "UTC",
		Prompt: "summarize", OriginChannel: "discord", OriginDestination: "default",
		OverlapPolicy: OverlapQueueOne, CatchUpGraceSeconds: 3600,
	}
	fixture := newScheduleFixture(t, spec, nil)
	active := Occurrence{
		ID: "cron-active", JobID: fixture.job.ID, JobVersion: 1,
		ScheduledForUTC: time.Date(2026, 9, 12, 11, 0, 0, 0, time.UTC),
		State:           OccurrenceFired, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	fixture.store.mu.Lock()
	fixture.store.occurrences[active.ID] = active
	fixture.store.mu.Unlock()
	startScheduleRun(t, fixture, fixture.job.ID)
	time.Sleep(300 * time.Millisecond)
	service, err := NewService(fixture.store)
	if err != nil {
		t.Fatalf("NewService(): %v", err)
	}
	if err := service.SetState(t.Context(), fixture.job.ID, JobPaused); err != nil {
		t.Fatalf("pause: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		fixture.clock.Advance(5 * time.Second)
		time.Sleep(5 * time.Millisecond)
		status, err := fixture.runtime.Status(t.Context(), durable.RunRef{Key: fixture.job.ID})
		if err == nil && status.State == durable.RunSucceeded {
			break
		}
	}
	status, err := fixture.runtime.Status(t.Context(), durable.RunRef{Key: fixture.job.ID})
	if err != nil {
		t.Fatalf("Status(): %v", err)
	}
	if status.State != durable.RunSucceeded {
		t.Errorf("paused wait run state = %q, want succeeded (clean exit, no retry storm)", status.State)
	}
	if fixture.turns.callCount() != 0 {
		t.Error("paused wait ran a turn")
	}
	fixture.store.mu.Lock()
	rows := len(fixture.store.occurrences)
	fixture.store.mu.Unlock()
	if rows != 1 {
		t.Errorf("occurrences = %d, want only the seeded active row", rows)
	}
}

func TestRunner_FailedTurnNotifiesWarning(t *testing.T) {
	fixture := newScheduleFixture(t, nil, []turnScript{
		{result: TurnResult{TurnID: "turn-9", Succeeded: false, SafeErrorCode: "model_failed"}},
	})
	startScheduleRun(t, fixture, fixture.job.ID)
	failed := waitOccurrenceState(t, fixture, fixture.job.ID, OccurrenceFailed)
	_ = failed
	if len(fixture.notify.calls) != 1 || fixture.notify.calls[0].Priority != PriorityWarning {
		t.Errorf("failure notification = %+v", fixture.notify.calls)
	}
}

func TestRunner_OverloadRetriesSameFire(t *testing.T) {
	fixture := newScheduleFixture(t, nil, []turnScript{
		{err: Errorf(ErrorCodeRuntimeOverloaded, "pending turn queue is full")},
		{result: TurnResult{TurnID: "turn-2", Text: "recovered", Succeeded: true}},
	})
	startScheduleRun(t, fixture, fixture.job.ID)
	completed := waitOccurrenceState(t, fixture, fixture.job.ID, OccurrenceCompleted)
	_ = completed
	if fixture.turns.callCount() != 2 {
		t.Fatalf("turn calls = %d, want overload retry", fixture.turns.callCount())
	}
	fixture.store.mu.Lock()
	count := len(fixture.store.occurrences)
	fixture.store.mu.Unlock()
	if count != 1 {
		t.Errorf("occurrence rows = %d, want 1 (no duplicate fire)", count)
	}
}

func TestRunner_TransportErrorFailsRun(t *testing.T) {
	fixture := newScheduleFixture(t, nil, []turnScript{
		{err: errors.New("engine unavailable")},
	})
	startScheduleRun(t, fixture, fixture.job.ID)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		fixture.clock.Advance(100 * time.Millisecond)
		time.Sleep(time.Millisecond)
		status, err := fixture.runtime.Status(t.Context(), durable.RunRef{Key: fixture.job.ID})
		if err == nil && status.State == durable.RunFailed {
			return
		}
	}
	t.Fatal("run did not fail on transport error")
}

func TestRunner_PausedJobExitsQuietly(t *testing.T) {
	fixture := newScheduleFixture(t, nil, nil)
	service, err := NewService(fixture.store)
	if err != nil {
		t.Fatalf("NewService(): %v", err)
	}
	if err := service.SetState(t.Context(), fixture.job.ID, JobPaused); err != nil {
		t.Fatalf("pause: %v", err)
	}
	startScheduleRun(t, fixture, fixture.job.ID)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		fixture.clock.Advance(100 * time.Millisecond)
		time.Sleep(time.Millisecond)
	}
	if fixture.turns.callCount() != 0 {
		t.Error("paused job ran a turn")
	}
	fixture.store.mu.Lock()
	rows := len(fixture.store.occurrences)
	fixture.store.mu.Unlock()
	if rows != 0 {
		t.Errorf("paused job recorded %d occurrences", rows)
	}
}

func TestRunner_MissingJobExits(t *testing.T) {
	fixture := newScheduleFixture(t, nil, nil)
	startScheduleRun(t, fixture, "job-ghost")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		fixture.clock.Advance(100 * time.Millisecond)
		time.Sleep(time.Millisecond)
	}
	if fixture.turns.callCount() != 0 {
		t.Error("missing job ran a turn")
	}
}

func TestRunner_RejectsBadWiring(t *testing.T) {
	store := newFakeJobStore()
	turns := &fakeTurnRunner{}
	notify := &fakeNotifier{}
	if _, err := NewRunner(nil, turns, notify, nil); err == nil {
		t.Error("nil store accepted")
	}
	if _, err := NewRunner(store, nil, notify, nil); err == nil {
		t.Error("nil turns accepted")
	}
	if _, err := NewRunner(store, turns, nil, nil); err == nil {
		t.Error("nil notifier accepted")
	}
	var nilCtx context.Context
	runner, err := NewRunner(store, turns, notify, nil)
	if err != nil {
		t.Fatalf("NewRunner(): %v", err)
	}
	if err := runner.Handle(nilCtx, nil); err == nil {
		t.Error("nil context accepted")
	}
}
