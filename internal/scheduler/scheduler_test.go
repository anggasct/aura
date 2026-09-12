package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeJobStore struct {
	mu          sync.Mutex
	jobs        map[string]Job
	occurrences map[string]Occurrence
	byFire      map[string]string
}

func newFakeJobStore() *fakeJobStore {
	return &fakeJobStore{jobs: map[string]Job{}, occurrences: map[string]Occurrence{}, byFire: map[string]string{}}
}

func (s *fakeJobStore) InsertJob(_ context.Context, job *Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[job.ID]; ok {
		return Errorf(ErrorCodeOccurrenceConflict, "job already exists")
	}
	s.jobs[job.ID] = *job
	return nil
}

func (s *fakeJobStore) Job(_ context.Context, id string) (Job, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	return job, ok, nil
}

func (s *fakeJobStore) EditJob(_ context.Context, id string, edit *JobEdit, expectedVersion int64, now time.Time) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.jobs[id]
	if !ok {
		return Job{}, Errorf(ErrorCodeJobNotFound, "schedule job not found")
	}
	if current.Version != expectedVersion || current.State == JobDeleted {
		return Job{}, Errorf(ErrorCodeOccurrenceConflict, "schedule job changed underneath the edit")
	}
	current.Name = edit.Name
	current.CronExpression = edit.CronExpression
	current.Timezone = edit.Timezone
	current.Prompt = edit.Prompt
	current.OriginChannel = edit.OriginChannel
	current.OriginDestination = edit.OriginDestination
	current.OverlapPolicy = edit.OverlapPolicy
	current.CatchUpGraceSeconds = edit.CatchUpGraceSeconds
	current.Version++
	current.UpdatedAt = now
	s.jobs[id] = current
	return current, nil
}

func (s *fakeJobStore) SetJobState(_ context.Context, id, state string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.jobs[id]
	if !ok {
		return Errorf(ErrorCodeJobNotFound, "schedule job not found")
	}
	if current.State == JobDeleted {
		return Errorf(ErrorCodeOccurrenceConflict, "schedule job state transition is not allowed")
	}
	current.State = state
	current.UpdatedAt = now
	s.jobs[id] = current
	return nil
}

func (s *fakeJobStore) RecordFire(_ context.Context, occurrence *Occurrence) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := occurrence.JobID + "|" + occurrence.ScheduledForUTC.UTC().Format(time.RFC3339Nano)
	if _, ok := s.byFire[key]; ok {
		return Errorf(ErrorCodeOccurrenceConflict, "occurrence already recorded for this fire")
	}
	s.occurrences[occurrence.ID] = *occurrence
	s.byFire[key] = occurrence.ID
	return nil
}

func (s *fakeJobStore) OccurrenceByFire(_ context.Context, jobID string, scheduledFor time.Time) (Occurrence, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byFire[jobID+"|"+scheduledFor.UTC().Format(time.RFC3339Nano)]
	if !ok {
		return Occurrence{}, false, nil
	}
	return s.occurrences[id], true, nil
}

func testJobSpec() *JobSpec {
	return &JobSpec{
		Name: "nightly summary", CronExpression: "0 21 * * *", Timezone: "UTC",
		Prompt: "summarize today", OriginChannel: "discord", OriginDestination: "default",
		OverlapPolicy: OverlapSkip, CatchUpGraceSeconds: 900,
	}
}

func testService() *Service {
	service, err := NewService(newFakeJobStore())
	if err != nil {
		panic(err)
	}
	return service
}

func TestService_CreateAndGet(t *testing.T) {
	service := testService()
	created, err := service.Create(t.Context(), testJobSpec())
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}
	if created.ID == "" || created.Version != 1 || created.State != JobActive {
		t.Errorf("created job = %+v", created)
	}
	fetched, err := service.Get(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if fetched.ID != created.ID || fetched.CronExpression != "0 21 * * *" {
		t.Errorf("fetched job mismatch: %+v", fetched)
	}
	if _, err := service.Get(t.Context(), "missing"); err == nil {
		t.Error("missing job accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeJobNotFound {
		t.Errorf("missing code = %v, %v", code, ok)
	}
}

func TestService_CreateRejectsInvalid(t *testing.T) {
	service := testService()
	valid := testJobSpec
	cases := []struct {
		name   string
		mutate func(*JobSpec)
		code   ErrorCode
	}{
		{"empty name", func(spec *JobSpec) { spec.Name = "" }, ErrorCodeInvalidArgument},
		{"bad expression", func(spec *JobSpec) { spec.CronExpression = "nope" }, ErrorCodeExpressionInvalid},
		{"bad timezone", func(spec *JobSpec) { spec.Timezone = "Mars/Olympus" }, ErrorCodeTimezoneInvalid},
		{"empty prompt", func(spec *JobSpec) { spec.Prompt = "" }, ErrorCodeInvalidArgument},
		{"empty channel", func(spec *JobSpec) { spec.OriginChannel = "" }, ErrorCodeInvalidArgument},
		{"bad policy", func(spec *JobSpec) { spec.OverlapPolicy = "parallel" }, ErrorCodeInvalidArgument},
		{"negative grace", func(spec *JobSpec) { spec.CatchUpGraceSeconds = -1 }, ErrorCodeInvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := valid()
			tc.mutate(spec)
			if _, err := service.Create(t.Context(), spec); err == nil {
				t.Errorf("invalid spec accepted: %+v", spec)
			} else if code, ok := CodeOf(err); !ok || code != tc.code {
				t.Errorf("code = %v, %v; want %v", code, ok, tc.code)
			}
		})
	}
	if _, err := service.Create(t.Context(), nil); err == nil {
		t.Error("nil spec accepted")
	}
	var nilCtx context.Context
	if _, err := service.Create(nilCtx, valid()); err == nil {
		t.Error("nil context accepted")
	}
}

func TestService_EditVersionsAndGuardsDeleted(t *testing.T) {
	service := testService()
	created, err := service.Create(t.Context(), testJobSpec())
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}
	edit := &JobEdit{
		Name: "renamed", CronExpression: "30 22 * * *", Timezone: "America/New_York",
		Prompt: "summarize", OriginChannel: "discord", OriginDestination: "default",
		OverlapPolicy: OverlapQueueOne, CatchUpGraceSeconds: 300,
	}
	updated, err := service.Edit(t.Context(), created.ID, edit)
	if err != nil {
		t.Fatalf("Edit(): %v", err)
	}
	if updated.Version != 2 || updated.Name != "renamed" || updated.OverlapPolicy != OverlapQueueOne {
		t.Errorf("edited job = %+v", updated)
	}
	if err := service.SetState(t.Context(), created.ID, JobDeleted); err != nil {
		t.Fatalf("SetState(): %v", err)
	}
	if _, err := service.Edit(t.Context(), created.ID, edit); err == nil {
		t.Error("edit of deleted job accepted")
	}
	if err := service.SetState(t.Context(), created.ID, JobActive); err == nil {
		t.Error("resurrect of deleted job accepted")
	}
}

func TestService_SetStateTransitions(t *testing.T) {
	service := testService()
	created, err := service.Create(t.Context(), testJobSpec())
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}
	if err := service.SetState(t.Context(), created.ID, JobPaused); err != nil {
		t.Fatalf("pause: %v", err)
	}
	paused, _ := service.Get(t.Context(), created.ID)
	if paused.State != JobPaused {
		t.Errorf("state = %q", paused.State)
	}
	if err := service.SetState(t.Context(), created.ID, JobActive); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if err := service.SetState(t.Context(), created.ID, "flying"); err == nil {
		t.Error("invalid state accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeJobStateInvalid {
		t.Errorf("code = %v, %v", code, ok)
	}
}

func TestService_NextFireAndRecordFire(t *testing.T) {
	service := testService()
	created, err := service.Create(t.Context(), testJobSpec())
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}
	fire, err := service.NextFire(t.Context(), created.ID, time.Date(2026, 9, 12, 20, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NextFire(): %v", err)
	}
	if !fire.Equal(time.Date(2026, 9, 12, 21, 0, 0, 0, time.UTC)) {
		t.Errorf("fire = %v", fire)
	}
	first, replayed, err := service.RecordFire(t.Context(), created.ID, fire, created.Version)
	if err != nil || replayed {
		t.Fatalf("RecordFire() = %+v, %v, %v", first.ID, replayed, err)
	}
	if first.State != OccurrenceFired || first.JobVersion != 1 {
		t.Errorf("occurrence = %+v", first)
	}
	second, replayed, err := service.RecordFire(t.Context(), created.ID, fire, created.Version)
	if err != nil || !replayed || second.ID != first.ID {
		t.Errorf("refire = %+v, %v, %v; want convergent replay", second.ID, replayed, err)
	}
}

func TestService_RunNowUsesCurrentTime(t *testing.T) {
	service := testService()
	created, err := service.Create(t.Context(), testJobSpec())
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}
	before := time.Now().UTC()
	occurrence, err := service.RunNow(t.Context(), created.ID, before)
	if err != nil {
		t.Fatalf("RunNow(): %v", err)
	}
	if occurrence.ScheduledForUTC.Before(before.Truncate(time.Second)) {
		t.Errorf("manual fire = %v", occurrence.ScheduledForUTC)
	}
	if _, err := service.RunNow(t.Context(), "missing", time.Now().UTC()); err == nil {
		t.Error("run-now on missing job accepted")
	}
	if err := service.SetState(t.Context(), created.ID, JobDeleted); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := service.RunNow(t.Context(), created.ID, time.Now().UTC()); err == nil {
		t.Error("run-now on deleted job accepted")
	}
}

func TestService_RunNowCreatesDistinctOccurrences(t *testing.T) {
	service := testService()
	created, err := service.Create(t.Context(), testJobSpec())
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}
	stamp := time.Date(2026, 9, 12, 21, 0, 0, 0, time.UTC)
	first, err := service.RunNow(t.Context(), created.ID, stamp)
	if err != nil {
		t.Fatalf("first RunNow(): %v", err)
	}
	second, err := service.RunNow(t.Context(), created.ID, stamp)
	if err != nil {
		t.Fatalf("second RunNow(): %v", err)
	}
	if first.ID == second.ID {
		t.Errorf("manual invocations converged on %q", first.ID)
	}
	if first.ScheduledForUTC.Equal(second.ScheduledForUTC) {
		t.Error("manual invocations share the same fire time")
	}
	scheduled, _, err := service.RecordFire(t.Context(), created.ID, stamp, created.Version)
	if err != nil {
		t.Fatalf("RecordFire(): %v", err)
	}
	if first.ID == scheduled.ID || second.ID == scheduled.ID {
		t.Error("manual invocation converged with a scheduled fire")
	}
}
