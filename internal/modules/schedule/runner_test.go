package schedule

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/principal"
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

func (s *fakeJobStore) AttachTurn(_ context.Context, id, turnID string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes = append(s.writes, "attach")
	occurrence, ok := s.occurrences[id]
	if !ok {
		return Errorf(ErrorCodeJobNotFound, "occurrence not found")
	}
	if occurrence.State != OccurrenceFired {
		return Errorf(ErrorCodeOccurrenceConflict, "occurrence is no longer fired")
	}
	occurrence.TurnID = turnID
	occurrence.UpdatedAt = now
	s.occurrences[id] = occurrence
	return nil
}

func (s *fakeJobStore) SettleOccurrence(ctx context.Context, id, state, turnID, _, _ string, now time.Time) error {
	s.mu.Lock()
	hook := s.settleHook
	s.mu.Unlock()
	if hook != nil {
		if err := hook(ctx); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes = append(s.writes, "settle:"+state)
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
	occurrence.UpdatedAt = now
	s.occurrences[id] = occurrence
	return nil
}

func (s *fakeJobStore) PruneOccurrences(_ context.Context, jobID string, before time.Time, limit int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pruned := 0
	for id := range s.occurrences {
		if pruned >= limit {
			break
		}
		occurrence := s.occurrences[id]
		if occurrence.JobID != jobID || occurrence.State == OccurrenceFired {
			continue
		}
		if occurrence.UpdatedAt.Before(before) {
			delete(s.occurrences, id)
			pruned++
		}
	}
	return pruned, nil
}

func (s *fakeJobStore) FiredOccurrences(_ context.Context, limit int) ([]Occurrence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var fired []Occurrence
	for id := range s.occurrences {
		if s.occurrences[id].State == OccurrenceFired {
			fired = append(fired, s.occurrences[id])
		}
	}
	slices.SortFunc(fired, func(a, b Occurrence) int { return a.ScheduledForUTC.Compare(b.ScheduledForUTC) })
	return fired[:min(limit, len(fired))], nil
}

func (s *fakeJobStore) ActiveJobs(_ context.Context, limit int) ([]Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var active []Job
	for id := range s.jobs {
		if s.jobs[id].State == JobActive {
			active = append(active, s.jobs[id])
		}
	}
	slices.SortFunc(active, func(a, b Job) int { return strings.Compare(a.ID, b.ID) })
	return active[:min(limit, len(active))], nil
}

func (s *fakeJobStore) seed(occurrence *Occurrence) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.occurrences[occurrence.ID] = *occurrence
	s.byFire[occurrence.JobID+"|"+occurrence.ScheduledForUTC.UTC().Format(time.RFC3339Nano)] = occurrence.ID
}

func (s *fakeJobStore) remove(jobID string, instant time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := jobID + "|" + instant.UTC().Format(time.RFC3339Nano)
	delete(s.occurrences, s.byFire[key])
	delete(s.byFire, key)
}

func (s *fakeJobStore) stateAt(jobID string, instant time.Time) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byFire[jobID+"|"+instant.UTC().Format(time.RFC3339Nano)]
	if !ok {
		return ""
	}
	return s.occurrences[id].State
}

func (s *fakeJobStore) recordedWrites() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.writes)
}

func (s *fakeJobStore) reads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobReads
}

func (s *fakeJobStore) setSettleHook(hook func(ctx context.Context) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settleHook = hook
}

func settles(writes []string) int {
	count := 0
	for _, write := range writes {
		if strings.HasPrefix(write, "settle:") {
			count++
		}
	}
	return count
}

type turnScript struct {
	result TurnResult
	err    error
}

type fakeTurnRunner struct {
	mu      sync.Mutex
	scripts []turnScript
	calls   []TurnRequest
	respond func(ctx context.Context, call int) (TurnResult, error)
}

func (r *fakeTurnRunner) RunTurn(ctx context.Context, req *TurnRequest) (TurnResult, error) {
	r.mu.Lock()
	r.calls = append(r.calls, *req)
	call := len(r.calls)
	respond := r.respond
	if respond == nil {
		defer r.mu.Unlock()
		if len(r.scripts) == 0 {
			return TurnResult{TurnID: "turn-" + req.IdempotencyKey, Text: "done", Succeeded: true}, nil
		}
		script := r.scripts[0]
		r.scripts = r.scripts[1:]
		return script.result, script.err
	}
	r.mu.Unlock()
	return respond(ctx, call)
}

func (r *fakeTurnRunner) setRespond(respond func(ctx context.Context, call int) (TurnResult, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.respond = respond
}

func (r *fakeTurnRunner) requests() []TurnRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

func (r *fakeTurnRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func alwaysOverloaded(context.Context, int) (TurnResult, error) {
	return TurnResult{}, Errorf(ErrorCodeRuntimeOverloaded, "pending turn queue is full")
}

type fakeNotifier struct {
	mu      sync.Mutex
	calls   []NotifyRequest
	err     error
	respond func(ctx context.Context, call int) error
}

func (n *fakeNotifier) Notify(ctx context.Context, req *NotifyRequest) error {
	n.mu.Lock()
	n.calls = append(n.calls, *req)
	call := len(n.calls)
	respond, err := n.respond, n.err
	n.mu.Unlock()
	if respond != nil {
		return respond(ctx, call)
	}
	return err
}

func (n *fakeNotifier) setRespond(respond func(ctx context.Context, call int) error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.respond = respond
}

func (n *fakeNotifier) notices() []NotifyRequest {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.calls)
}

type recordingStarter struct {
	mu       sync.Mutex
	requests []durable.StartRequest
	fail     string
}

func (s *recordingStarter) Start(_ context.Context, req durable.StartRequest) (durable.RunRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != "" && strings.HasPrefix(req.Key, HandlerName+"/"+s.fail+"/") {
		return durable.RunRef{}, errors.New("start refused")
	}
	s.requests = append(s.requests, req)
	return durable.RunRef{Key: req.Key}, nil
}

func (s *recordingStarter) keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.requests))
	for _, req := range s.requests {
		keys = append(keys, req.Key)
	}
	return keys
}

func (s *recordingStarter) payloads(t *testing.T) []firePayload {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	fires := make([]firePayload, 0, len(s.requests))
	for _, req := range s.requests {
		var fire firePayload
		if err := json.Unmarshal(req.Payload, &fire); err != nil {
			t.Fatalf("payload of %s: %v", req.Key, err)
		}
		fires = append(fires, fire)
	}
	return fires
}

type recordingLogs struct {
	mu       sync.Mutex
	warnings []string
}

func (l *recordingLogs) Enabled(context.Context, slog.Level) bool { return true }

func (l *recordingLogs) Handle(_ context.Context, record slog.Record) error {
	if record.Level != slog.LevelWarn {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warnings = append(l.warnings, record.Message)
	return nil
}

func (l *recordingLogs) WithAttrs([]slog.Attr) slog.Handler { return l }

func (l *recordingLogs) WithGroup(string) slog.Handler { return l }

func (l *recordingLogs) warned() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.warnings)
}

func defaultRunSpec() *JobSpec {
	return &JobSpec{
		Name: "nightly", CronExpression: "* * * * *", Timezone: "UTC",
		Prompt: "summarize", OriginChannel: "discord", OriginDestination: "default",
		OverlapPolicy: OverlapSkip, CatchUpGraceSeconds: 900,
	}
}

type runFixture struct {
	runner  *Runner
	store   *fakeJobStore
	turns   *fakeTurnRunner
	notify  *fakeNotifier
	starter *recordingStarter
	logs    *recordingLogs
	runtime *durable.Fake
	service *Service
	job     Job
	horizon time.Time
}

func newRunFixture(t *testing.T, mutate func(*JobSpec)) *runFixture {
	t.Helper()
	spec := defaultRunSpec()
	if mutate != nil {
		mutate(spec)
	}
	f := &runFixture{store: newFakeJobStore(), turns: &fakeTurnRunner{}, notify: &fakeNotifier{}, starter: &recordingStarter{}, logs: &recordingLogs{}, runtime: durable.NewFake()}
	runner, err := NewRunner(&RunnerDeps{Jobs: f.store, Turns: f.turns, Notify: f.notify, Starter: f.starter, Logger: slog.New(f.logs)})
	if err != nil {
		t.Fatalf("NewRunner(): %v", err)
	}
	f.runner = runner
	f.runtime.RegisterHandler(HandlerName, runner.Handle)
	t.Cleanup(f.runtime.Close)
	f.service, err = NewService(f.store)
	if err != nil {
		t.Fatalf("NewService(): %v", err)
	}
	f.job, err = f.service.Create(t.Context(), spec)
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}
	return f
}

func inBubble(t *testing.T, mutate func(*JobSpec), body func(t *testing.T, f *runFixture)) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		body(t, newRunFixture(t, mutate))
	})
}

func (f *runFixture) nextFire(t *testing.T) time.Time {
	t.Helper()
	fire, err := nextFireOf(&f.job, time.Now())
	if err != nil {
		t.Fatalf("next fire: %v", err)
	}
	return fire
}

func (f *runFixture) scheduled(t *testing.T, instant time.Time) string {
	t.Helper()
	return f.launch(t, &FireRequest{JobID: f.job.ID, Instant: instant, Chain: true, Expression: f.job.CronExpression, Timezone: f.job.Timezone})
}

func (f *runFixture) recorded(t *testing.T, instant time.Time) string {
	t.Helper()
	return f.launch(t, &FireRequest{JobID: f.job.ID, Instant: instant})
}

func (f *runFixture) launch(t *testing.T, req *FireRequest) string {
	t.Helper()
	req.Starter = f.runtime
	if err := StartFire(t.Context(), req); err != nil {
		t.Fatalf("StartFire(): %v", err)
	}
	if req.Instant.After(f.horizon) {
		f.horizon = req.Instant
	}
	return fireKey(req.JobID, req.Instant)
}

func (f *runFixture) finish(t *testing.T, key string) durable.RunStatus {
	t.Helper()
	ended := make(chan struct{})
	go func() {
		f.runtime.WaitReady(durable.RunRef{Key: key})
		close(ended)
	}()
	select {
	case <-ended:
	case <-time.After(max(time.Until(f.horizon), 0) + 48*time.Hour):
		t.Fatalf("run %s did not end", key)
	}
	status, err := f.runtime.Status(t.Context(), durable.RunRef{Key: key})
	if err != nil {
		t.Fatalf("Status(%s): %v", key, err)
	}
	return status
}

func (f *runFixture) succeed(t *testing.T, key string) {
	t.Helper()
	if status := f.finish(t, key); status.State != durable.RunSucceeded {
		t.Fatalf("run %s = %s (%s), want succeeded", key, status.State, status.Detail)
	}
}

type firedRow struct {
	jobID   string
	instant time.Time
	created time.Time
	state   string
}

func (f *runFixture) seed(row firedRow) Occurrence {
	jobID := cmp.Or(row.jobID, f.job.ID)
	state := cmp.Or(row.state, OccurrenceFired)
	occurrence := Occurrence{
		ID:    occurrenceID(jobID, row.instant) + "-" + strings.ReplaceAll(row.instant.Format("05.000000000"), ".", ""),
		JobID: jobID, JobVersion: 1, ScheduledForUTC: row.instant.UTC(), State: state,
		CreatedAt: row.created, UpdatedAt: row.created,
	}
	f.store.seed(&occurrence)
	return occurrence
}

func (f *runFixture) setState(t *testing.T, state string) {
	t.Helper()
	if err := f.service.SetState(t.Context(), f.job.ID, state); err != nil {
		t.Fatalf("SetState(%s): %v", state, err)
	}
}

func (f *runFixture) edit(t *testing.T, mutate func(*JobEdit)) {
	t.Helper()
	current, err := f.service.Get(t.Context(), f.job.ID)
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	edit := &JobEdit{
		Name: current.Name, CronExpression: current.CronExpression, Timezone: current.Timezone,
		Prompt: current.Prompt, OriginChannel: current.OriginChannel, OriginDestination: current.OriginDestination,
		OverlapPolicy: current.OverlapPolicy, CatchUpGraceSeconds: current.CatchUpGraceSeconds,
	}
	mutate(edit)
	if _, err := f.service.Edit(t.Context(), f.job.ID, edit); err != nil {
		t.Fatalf("Edit(): %v", err)
	}
}

func (f *runFixture) settleAsOtherWriter(ctx context.Context, instant time.Time) error {
	f.store.mu.Lock()
	id := f.store.byFire[f.job.ID+"|"+instant.UTC().Format(time.RFC3339Nano)]
	f.store.mu.Unlock()
	return f.store.SettleOccurrence(ctx, id, OccurrenceCancelled, "", "", "", time.Now())
}

type rowExpectation struct {
	jobID string
	state string
	turns int
	next  int
}

func (f *runFixture) expectRow(t *testing.T, instant time.Time, want rowExpectation) {
	t.Helper()
	if got := f.store.stateAt(cmp.Or(want.jobID, f.job.ID), instant); got != want.state {
		t.Errorf("occurrence state = %q, want %q", got, want.state)
	}
	if got := f.turns.callCount(); got != want.turns {
		t.Errorf("turn submissions = %d, want %d", got, want.turns)
	}
	if got := f.starter.keys(); len(got) != want.next {
		t.Errorf("successor starts = %v, want %d", got, want.next)
	}
}

type fireCase struct {
	name    string
	mutate  func(*JobSpec)
	prepare func(t *testing.T, f *runFixture) (string, time.Time)
	want    rowExpectation
}

func TestOccurrenceRunDrivesAFiredRowWhateverTheJobState(t *testing.T) {
	cases := []fireCase{
		{name: "a paused job", want: rowExpectation{state: OccurrenceCompleted, turns: 1}, prepare: func(t *testing.T, f *runFixture) (string, time.Time) {
			f.seed(firedRow{instant: time.Now(), created: time.Now()})
			f.setState(t, JobPaused)
			return f.scheduled(t, time.Now()), time.Now()
		}},
		{name: "a deleted job", want: rowExpectation{state: OccurrenceCancelled}, prepare: func(t *testing.T, f *runFixture) (string, time.Time) {
			f.seed(firedRow{instant: time.Now(), created: time.Now()})
			f.setState(t, JobDeleted)
			return f.scheduled(t, time.Now()), time.Now()
		}},
		{name: "a missing job", want: rowExpectation{jobID: "job-ghost", state: OccurrenceCancelled}, prepare: func(t *testing.T, f *runFixture) (string, time.Time) {
			f.seed(firedRow{jobID: "job-ghost", instant: time.Now(), created: time.Now()})
			return f.launch(t, &FireRequest{JobID: "job-ghost", Instant: time.Now(), Chain: true, Expression: "* * * * *", Timezone: "UTC"}), time.Now()
		}},
		{name: "a scheduled row recorded longer ago than the grace", want: rowExpectation{state: OccurrenceExpired, next: 1}, prepare: func(t *testing.T, f *runFixture) (string, time.Time) {
			old := time.Now().Add(-2 * time.Hour)
			f.seed(firedRow{instant: old, created: old})
			return f.scheduled(t, old), old
		}},
		{name: "a scheduled row recorded after a queue_one wait", want: rowExpectation{state: OccurrenceCompleted, turns: 1, next: 1}, prepare: func(t *testing.T, f *runFixture) (string, time.Time) {
			old := time.Now().Add(-2 * time.Hour)
			f.seed(firedRow{instant: old, created: time.Now()})
			return f.scheduled(t, old), old
		}},
		{name: "a manual row older than the grace", want: rowExpectation{state: OccurrenceCompleted, turns: 1, next: 1}, prepare: func(t *testing.T, f *runFixture) (string, time.Time) {
			manual := time.Now().Add(-2 * time.Hour).Add(7 * time.Nanosecond)
			f.seed(firedRow{instant: manual, created: manual})
			return f.recorded(t, manual), manual
		}},
		{name: "another occurrence active under skip", want: rowExpectation{state: OccurrenceCompleted, turns: 1, next: 1}, prepare: func(t *testing.T, f *runFixture) (string, time.Time) {
			f.seed(firedRow{instant: time.Now().Add(-time.Hour), created: time.Now()})
			f.seed(firedRow{instant: time.Now(), created: time.Now()})
			return f.scheduled(t, time.Now()), time.Now()
		}},
		{name: "a row another writer settles during the turn", want: rowExpectation{state: OccurrenceCancelled, turns: 1, next: 1}, prepare: func(t *testing.T, f *runFixture) (string, time.Time) {
			instant := time.Now()
			f.seed(firedRow{instant: instant, created: instant})
			f.turns.setRespond(func(ctx context.Context, _ int) (TurnResult, error) {
				return TurnResult{TurnID: "turn-1", Text: "done", Succeeded: true}, f.settleAsOtherWriter(ctx, instant)
			})
			return f.scheduled(t, instant), instant
		}},
		{name: "a row another writer settles during the notification", want: rowExpectation{state: OccurrenceCancelled, turns: 1, next: 1}, prepare: func(t *testing.T, f *runFixture) (string, time.Time) {
			instant := time.Now()
			f.seed(firedRow{instant: instant, created: instant})
			f.notify.setRespond(func(ctx context.Context, _ int) error { return f.settleAsOtherWriter(ctx, instant) })
			return f.scheduled(t, instant), instant
		}},
		{name: "a row another invocation already settled", want: rowExpectation{state: OccurrenceCompleted, next: 1}, prepare: func(t *testing.T, f *runFixture) (string, time.Time) {
			f.seed(firedRow{instant: time.Now(), created: time.Now(), state: OccurrenceCompleted})
			return f.scheduled(t, time.Now()), time.Now()
		}},
	}
	runFireCases(t, cases)
}

func runFireCases(t *testing.T, cases []fireCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inBubble(t, tc.mutate, func(t *testing.T, f *runFixture) {
				key, instant := tc.prepare(t, f)
				f.succeed(t, key)
				f.expectRow(t, instant, tc.want)
			})
		})
	}
}

func TestOccurrenceRunEndsBeforeARowWhenTheJobIsPausedOrGone(t *testing.T) {
	queueOne := func(spec *JobSpec) { spec.OverlapPolicy = OverlapQueueOne }
	cases := []struct {
		name    string
		mutate  func(*JobSpec)
		prepare func(t *testing.T, f *runFixture) string
		writes  []string
		next    int
	}{
		{name: "a paused job", prepare: func(t *testing.T, f *runFixture) string {
			f.setState(t, JobPaused)
			return f.scheduled(t, time.Now())
		}},
		{name: "a deleted job", prepare: func(t *testing.T, f *runFixture) string {
			f.setState(t, JobDeleted)
			return f.scheduled(t, time.Now())
		}},
		{name: "a missing job", prepare: func(t *testing.T, f *runFixture) string {
			return f.launch(t, &FireRequest{JobID: "job-ghost", Instant: time.Now(), Chain: true, Expression: "* * * * *", Timezone: "UTC"})
		}},
		{name: "a job paused during a queue_one wait", mutate: queueOne, prepare: func(t *testing.T, f *runFixture) string {
			f.seed(firedRow{instant: time.Now().Add(-time.Hour), created: time.Now()})
			fire := f.nextFire(t)
			key := f.scheduled(t, fire)
			time.Sleep(time.Until(fire) + time.Second)
			synctest.Wait()
			f.setState(t, JobPaused)
			return key
		}},
		{name: "a recorded run without a row", prepare: func(t *testing.T, f *runFixture) string {
			return f.recorded(t, time.Now().Add(3*time.Nanosecond))
		}},
		{name: "a fire beyond the grace", writes: []string{"record:" + OccurrenceExpired}, next: 1, prepare: func(t *testing.T, f *runFixture) string {
			return f.scheduled(t, time.Now().Add(-2*time.Hour))
		}},
		{name: "a skipped fire", writes: []string{"record:" + OccurrenceSkippedOverlap}, next: 1, prepare: func(t *testing.T, f *runFixture) string {
			f.seed(firedRow{instant: time.Now().Add(-time.Hour), created: time.Now()})
			return f.scheduled(t, time.Now())
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inBubble(t, tc.mutate, func(t *testing.T, f *runFixture) {
				f.succeed(t, tc.prepare(t, f))
				if got := f.store.recordedWrites(); !slices.Equal(got, tc.writes) {
					t.Errorf("writes = %v, want %v", got, tc.writes)
				}
				if got := f.starter.keys(); len(got) != tc.next {
					t.Errorf("successor starts = %v, want %d", got, tc.next)
				}
				if calls := f.turns.callCount(); calls != 0 {
					t.Errorf("turn submissions = %d, want 0", calls)
				}
			})
		})
	}
}

func TestOccurrenceRunFollowsAnEdit(t *testing.T) {
	hourly := func(spec *JobSpec) { spec.CronExpression = "0 * * * *" }
	t.Run("the turn carries the current prompt", func(t *testing.T) {
		inBubble(t, hourly, func(t *testing.T, f *runFixture) {
			key := f.scheduled(t, f.nextFire(t))
			time.Sleep(30 * time.Minute)
			f.edit(t, func(edit *JobEdit) { edit.Prompt = "summarize again" })
			f.succeed(t, key)
			if requests := f.turns.requests(); len(requests) != 1 || requests[0].Prompt != "summarize again" {
				t.Errorf("turn requests = %+v, want one with the edited prompt", requests)
			}
		})
	})
	t.Run("an instant the edit removed gets no occurrence", func(t *testing.T) {
		inBubble(t, hourly, func(t *testing.T, f *runFixture) {
			fire := f.nextFire(t)
			key := f.scheduled(t, fire)
			f.edit(t, func(edit *JobEdit) { edit.CronExpression = "30 * * * *" })
			f.succeed(t, key)
			if state := f.store.stateAt(f.job.ID, fire); state != "" {
				t.Errorf("occurrence state = %q, want none", state)
			}
			if got, want := f.starter.keys(), []string{fireKey(f.job.ID, fire.Add(30*time.Minute))}; !slices.Equal(got, want) {
				t.Errorf("successor = %v, want %v", got, want)
			}
			if payloads := f.starter.payloads(t); payloads[0].Expression != "30 * * * *" {
				t.Errorf("successor payload = %+v, want the edited expression", payloads[0])
			}
		})
	})
	t.Run("an edit that keeps the instant still fires", func(t *testing.T) {
		inBubble(t, hourly, func(t *testing.T, f *runFixture) {
			fire := f.nextFire(t)
			key := f.scheduled(t, fire)
			f.edit(t, func(edit *JobEdit) { edit.CronExpression = "0,30 * * * *" })
			f.succeed(t, key)
			f.expectRow(t, fire, rowExpectation{state: OccurrenceCompleted, turns: 1, next: 1})
		})
	})
}

func TestOccurrenceRunFiresInADaylightSavingGap(t *testing.T) {
	gapDay := func(spec *JobSpec) {
		spec.CronExpression, spec.Timezone = "30 2 * * *", "America/New_York"
	}
	fire := time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC)
	t.Run("the gap-day fire of an unchanged job", func(t *testing.T) {
		inBubble(t, gapDay, func(t *testing.T, f *runFixture) {
			f.succeed(t, f.scheduled(t, fire))
			f.expectRow(t, fire, rowExpectation{state: OccurrenceCompleted, turns: 1, next: 1})
		})
	})
	t.Run("the same instant after an edit that does not yield it", func(t *testing.T) {
		inBubble(t, gapDay, func(t *testing.T, f *runFixture) {
			key := f.scheduled(t, fire)
			f.edit(t, func(edit *JobEdit) { edit.CronExpression = "0 4 * * *" })
			f.succeed(t, key)
			f.expectRow(t, fire, rowExpectation{state: "", turns: 0, next: 1})
		})
	})
}

func TestRecordedRunHandlesAManualOccurrence(t *testing.T) {
	for _, policy := range []string{OverlapSkip, OverlapQueueOne} {
		t.Run("a run-now occurrence under "+policy, func(t *testing.T) {
			inBubble(t, func(spec *JobSpec) { spec.OverlapPolicy = policy }, func(t *testing.T, f *runFixture) {
				occurrence, err := f.service.RunNow(t.Context(), f.job.ID, time.Now())
				if err != nil {
					t.Fatalf("RunNow(): %v", err)
				}
				f.succeed(t, f.recorded(t, occurrence.ScheduledForUTC))
				f.expectRow(t, occurrence.ScheduledForUTC, rowExpectation{state: OccurrenceCompleted, turns: 1, next: 1})
			})
		})
	}
	hourly := func(spec *JobSpec) { spec.CronExpression = "0 * * * *" }
	t.Run("a leftover row starts the fire after the current time", func(t *testing.T) {
		inBubble(t, hourly, func(t *testing.T, f *runFixture) {
			old := time.Now().Add(-48 * time.Hour)
			f.seed(firedRow{instant: old, created: old})
			want := []string{fireKey(f.job.ID, f.nextFire(t))}
			f.succeed(t, f.recorded(t, old))
			if got := f.starter.keys(); !slices.Equal(got, want) {
				t.Errorf("successor = %v, want %v", got, want)
			}
		})
	})
	t.Run("a leftover row starts none while another occurrence is fired", func(t *testing.T) {
		inBubble(t, hourly, func(t *testing.T, f *runFixture) {
			old := time.Now().Add(-48 * time.Hour)
			f.seed(firedRow{instant: old, created: old})
			f.seed(firedRow{instant: time.Now().Add(-time.Hour), created: time.Now()})
			f.succeed(t, f.recorded(t, old))
			f.expectRow(t, old, rowExpectation{state: OccurrenceExpired})
		})
	})
	t.Run("a leftover row of a paused job starts none", func(t *testing.T) {
		inBubble(t, hourly, func(t *testing.T, f *runFixture) {
			old := time.Now().Add(-48 * time.Hour)
			f.seed(firedRow{instant: old, created: old})
			f.setState(t, JobPaused)
			f.succeed(t, f.recorded(t, old))
			f.expectRow(t, old, rowExpectation{state: OccurrenceExpired})
		})
	})
	t.Run("two manual occurrences in one second", func(t *testing.T) {
		inBubble(t, nil, func(t *testing.T, f *runFixture) {
			now := time.Now()
			var keys []string
			var instants []time.Time
			for range 2 {
				occurrence, err := f.service.RunNow(t.Context(), f.job.ID, now)
				if err != nil {
					t.Fatalf("RunNow(): %v", err)
				}
				instants = append(instants, occurrence.ScheduledForUTC)
				keys = append(keys, f.recorded(t, occurrence.ScheduledForUTC))
			}
			for _, key := range keys {
				f.succeed(t, key)
			}
			requests := f.turns.requests()
			if keys[0] == keys[1] || len(requests) != 2 || requests[0].IdempotencyKey == requests[1].IdempotencyKey {
				t.Errorf("runs %v, turn requests %+v, want two of each with distinct keys", keys, requests)
			}
			for _, instant := range instants {
				if state := f.store.stateAt(f.job.ID, instant); state != OccurrenceCompleted {
					t.Errorf("occurrence %v = %q, want completed", instant, state)
				}
			}
		})
	})
	t.Run("a manual occurrence in the second of a scheduled fire", func(t *testing.T) {
		fire := time.Date(2026, 9, 12, 21, 0, 0, 0, time.UTC)
		scheduled, manual := turnKey("job-1", fire), turnKey("job-1", fire.Add(5*time.Nanosecond))
		if scheduled != "cron:job-1:20260912T210000" || manual != "cron:job-1:20260912T210000.000000005" {
			t.Errorf("turn keys = %q and %q", scheduled, manual)
		}
	})
}

func TestScheduledFireSkipsWhileAManualOccurrenceIsActive(t *testing.T) {
	inBubble(t, nil, func(t *testing.T, f *runFixture) {
		if _, err := f.service.RunNow(t.Context(), f.job.ID, time.Now()); err != nil {
			t.Fatalf("RunNow(): %v", err)
		}
		fire := f.nextFire(t)
		f.succeed(t, f.scheduled(t, fire))
		f.expectRow(t, fire, rowExpectation{state: OccurrenceSkippedOverlap, next: 1})
	})
}

func TestOverloadRetriesTheSameFireUnderANewAction(t *testing.T) {
	shortGrace := func(spec *JobSpec) { spec.CatchUpGraceSeconds = 60 }
	t.Run("an overload and then a result", func(t *testing.T) {
		inBubble(t, nil, func(t *testing.T, f *runFixture) {
			f.turns.scripts = []turnScript{
				{err: Errorf(ErrorCodeRuntimeOverloaded, "pending turn queue is full")},
				{result: TurnResult{TurnID: "turn-2", Text: "recovered", Succeeded: true}},
			}
			fire := f.nextFire(t)
			f.succeed(t, f.scheduled(t, fire))
			f.expectRow(t, fire, rowExpectation{state: OccurrenceCompleted, turns: 2, next: 1})
			if requests := f.turns.requests(); requests[0].IdempotencyKey != requests[1].IdempotencyKey {
				t.Errorf("idempotency keys = %q and %q, want one", requests[0].IdempotencyKey, requests[1].IdempotencyKey)
			}
			if count := settles(f.store.recordedWrites()); count != 1 {
				t.Errorf("settles = %d, want 1", count)
			}
		})
	})
	t.Run("a scheduled fire whose grace ends during the overload", func(t *testing.T) {
		inBubble(t, shortGrace, func(t *testing.T, f *runFixture) {
			f.turns.setRespond(alwaysOverloaded)
			fire := f.nextFire(t)
			f.succeed(t, f.scheduled(t, fire))
			f.expectRow(t, fire, rowExpectation{state: OccurrenceExpired, turns: 2, next: 1})
		})
	})
	t.Run("a manual occurrence measures the grace from its first submission", func(t *testing.T) {
		inBubble(t, shortGrace, func(t *testing.T, f *runFixture) {
			f.turns.setRespond(alwaysOverloaded)
			manual := time.Now().Add(-2 * time.Hour).Add(9 * time.Nanosecond)
			f.seed(firedRow{instant: manual, created: manual})
			f.succeed(t, f.recorded(t, manual))
			f.expectRow(t, manual, rowExpectation{state: OccurrenceExpired, turns: 2, next: 1})
		})
	})
}

func TestOccurrenceRunFailsOnATurnTransportError(t *testing.T) {
	inBubble(t, nil, func(t *testing.T, f *runFixture) {
		f.turns.scripts = []turnScript{{err: errors.New("engine unavailable")}}
		fire := f.nextFire(t)
		status := f.finish(t, f.scheduled(t, fire))
		if status.State != durable.RunFailed || !strings.Contains(status.Detail, "engine unavailable") {
			t.Errorf("run = %s (%s), want failed with the transport error", status.State, status.Detail)
		}
		f.expectRow(t, fire, rowExpectation{state: OccurrenceFired, turns: 1})
		if notices := f.notify.notices(); len(notices) != 0 {
			t.Errorf("notifications = %+v, want none", notices)
		}
	})
}

func TestOccurrenceRunGoesOnAfterANotifierError(t *testing.T) {
	inBubble(t, nil, func(t *testing.T, f *runFixture) {
		f.notify.err = errors.New("broadcast unavailable")
		fire := f.nextFire(t)
		f.succeed(t, f.scheduled(t, fire))
		f.expectRow(t, fire, rowExpectation{state: OccurrenceCompleted, turns: 1, next: 1})
		if warnings := f.logs.warned(); !slices.Equal(warnings, []string{"schedule notification failed"}) {
			t.Errorf("warnings = %v, want one notification warning", warnings)
		}
	})
}

type payloadInvocation struct {
	payload []byte
}

func (payloadInvocation) Run() durable.RunRef { return durable.RunRef{} }

func (p payloadInvocation) Payload() []byte { return p.payload }

func (payloadInvocation) Signal(context.Context, string) ([]byte, bool) { return nil, false }

func (payloadInvocation) Sleep(time.Duration) error { return nil }

func (payloadInvocation) Timer(time.Duration) <-chan time.Time { return nil }

func (payloadInvocation) Wait(context.Context, string, time.Duration) (payload []byte, timedOut, ok bool) {
	return nil, false, false
}

func (payloadInvocation) RunAction(context.Context, string, func(context.Context) ([]byte, error)) ([]byte, error) {
	return nil, errors.New("payload invocation does not run actions")
}

func TestRunner_RejectsBadWiring(t *testing.T) {
	store, turns, notify, starter := newFakeJobStore(), &fakeTurnRunner{}, &fakeNotifier{}, &recordingStarter{}
	for name, deps := range map[string]*RunnerDeps{
		"no dependencies": nil,
		"no store":        {Turns: turns, Notify: notify, Starter: starter},
		"no turns":        {Jobs: store, Notify: notify, Starter: starter},
		"no notifier":     {Jobs: store, Turns: turns, Starter: starter},
		"no starter":      {Jobs: store, Turns: turns, Notify: notify},
	} {
		if _, err := NewRunner(deps); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	runner, err := NewRunner(&RunnerDeps{Jobs: store, Turns: turns, Notify: notify, Starter: starter})
	if err != nil {
		t.Fatalf("NewRunner(): %v", err)
	}
	var nilCtx context.Context
	if err := runner.Handle(nilCtx, nil); err == nil {
		t.Error("nil context accepted")
	}
	for _, payload := range []string{"job-1", `{"instant":"2026-09-12T21:00:00Z"}`, `{"job_id":"job-1","instant":"2026-09-12T21:00:00Z","chain":true}`} {
		if err := runner.Handle(t.Context(), payloadInvocation{payload: []byte(payload)}); err == nil {
			t.Errorf("payload %s accepted", payload)
		} else if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
			t.Errorf("payload %s: code = %v, %v", payload, code, ok)
		}
	}
}

func TestRunner_WorkflowJobsPassTheirTargetInsteadOfAPrompt(t *testing.T) {
	workflowJob := func(spec *JobSpec) {
		spec.Prompt, spec.WorkflowID, spec.WorkflowInput = "", "routine_weekly_report", `{"objective":"last week"}`
	}
	inBubble(t, workflowJob, func(t *testing.T, f *runFixture) {
		f.succeed(t, f.scheduled(t, f.nextFire(t)))
		requests := f.turns.requests()
		if len(requests) != 1 {
			t.Fatalf("turn calls = %d, want 1", len(requests))
		}
		call := requests[0]
		if call.WorkflowID != "routine_weekly_report" || call.WorkflowInput != `{"objective":"last week"}` || call.Prompt != "" || call.IdempotencyKey == "" {
			t.Errorf("fire request = %+v", call)
		}
		if call.SessionID != "cron:"+f.job.ID || call.PrincipalID != principal.OwnerID || call.Origin != "schedule" {
			t.Errorf("fire request = %+v", call)
		}
	})
}

func TestJobSpecsNeedExactlyOneTarget(t *testing.T) {
	service, err := NewService(newFakeJobStore())
	if err != nil {
		t.Fatal(err)
	}
	base := JobSpec{Name: "n", CronExpression: "* * * * *", Timezone: "UTC", OriginChannel: "discord", OriginDestination: "default", OverlapPolicy: OverlapSkip}
	for label, mutate := range map[string]func(*JobSpec){
		"neither":            func(*JobSpec) {},
		"both":               func(s *JobSpec) { s.Prompt, s.WorkflowID = "p", "w" },
		"oversized workflow": func(s *JobSpec) { s.WorkflowID, s.WorkflowInput = "w", string(make([]byte, maxPromptBytes+1)) },
	} {
		spec := base
		mutate(&spec)
		if _, err := service.Create(t.Context(), &spec); err == nil {
			t.Errorf("%s: Create accepted %+v", label, spec)
		}
	}
	spec := base
	spec.WorkflowID = "routine_x"
	if _, err := service.Create(t.Context(), &spec); err != nil {
		t.Fatalf("workflow job: %v", err)
	}
}
