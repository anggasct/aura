package store

import (
	"testing"
	"time"
)

func testScheduledJob(id string) *ScheduledJob {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	return &ScheduledJob{
		ID: id, Name: "nightly", CronExpression: "0 21 * * *", Timezone: "UTC",
		Prompt: "summarize", OriginChannel: "discord", OriginDestination: "default",
		OverlapPolicy: ScheduleOverlapSkip, CatchUpGraceSeconds: 900,
		State: ScheduleJobActive, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
}

func TestScheduleStore_JobLifecycle(t *testing.T) {
	db := newTestDB(t)
	s := NewScheduleStore(db)
	ctx := t.Context()

	if err := s.InsertJob(ctx, testScheduledJob("job-1")); err != nil {
		t.Fatalf("InsertJob(): %v", err)
	}
	if err := s.InsertJob(ctx, testScheduledJob("job-1")); err == nil {
		t.Fatal("duplicate job accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeScheduleConflict {
		t.Errorf("duplicate code = %v, %v", code, ok)
	}
	job, err := s.Job(ctx, "job-1")
	if err != nil {
		t.Fatalf("Job(): %v", err)
	}
	if job.Name != "nightly" || job.Version != 1 || job.State != ScheduleJobActive {
		t.Errorf("job mismatch: %+v", job)
	}
	if _, err := s.Job(ctx, "missing"); err == nil {
		t.Error("missing job accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeScheduleNotFound {
		t.Errorf("missing code = %v, %v", code, ok)
	}

	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	edited, err := s.EditJob(ctx, "job-1", &JobEdit{
		Name: "renamed", CronExpression: "30 22 * * *", Timezone: "UTC",
		Prompt: "summarize", OriginChannel: "discord", OriginDestination: "default",
		OverlapPolicy: ScheduleOverlapQueueOne, CatchUpGraceSeconds: 300, ExpectedVersion: 1,
	}, now)
	if err != nil {
		t.Fatalf("EditJob(): %v", err)
	}
	if edited.Version != 2 || edited.Name != "renamed" {
		t.Errorf("edited job = %+v", edited)
	}
	if _, err := s.EditJob(ctx, "job-1", &JobEdit{
		Name: "stale", CronExpression: "0 21 * * *", Timezone: "UTC",
		Prompt: "summarize", OriginChannel: "discord", OriginDestination: "default",
		OverlapPolicy: ScheduleOverlapSkip, CatchUpGraceSeconds: 900, ExpectedVersion: 1,
	}, now); err == nil {
		t.Error("stale edit accepted")
	}

	if err := s.SetJobState(ctx, "job-1", ScheduleJobPaused, now); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := s.SetJobState(ctx, "job-1", ScheduleJobActive, now); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if err := s.SetJobState(ctx, "job-1", ScheduleJobDeleted, now); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.SetJobState(ctx, "job-1", ScheduleJobActive, now); err == nil {
		t.Error("resurrect accepted")
	}
	if err := s.SetJobState(ctx, "job-1", "flying", now); err == nil {
		t.Error("invalid state accepted")
	}
}

func TestScheduleStore_OccurrenceFireAndSettle(t *testing.T) {
	db := newTestDB(t)
	s := NewScheduleStore(db)
	ctx := t.Context()

	if err := s.InsertJob(ctx, testScheduledJob("job-1")); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	fire := time.Date(2026, 9, 12, 21, 0, 0, 0, time.UTC)
	now := time.Date(2026, 9, 12, 21, 0, 5, 0, time.UTC)
	occurrence := &ScheduledOccurrence{
		ID: "cron-job-1-1", JobID: "job-1", JobVersion: 1, ScheduledForUTC: fire,
		State: OccurrenceFired, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.RecordFire(ctx, occurrence); err != nil {
		t.Fatalf("RecordFire(): %v", err)
	}
	if err := s.RecordFire(ctx, occurrence); err == nil {
		t.Fatal("duplicate fire accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeScheduleConflict {
		t.Errorf("duplicate code = %v, %v", code, ok)
	}
	found, ok, err := s.OccurrenceByFire(ctx, "job-1", fire)
	if err != nil || !ok || found.ID != "cron-job-1-1" {
		t.Errorf("OccurrenceByFire() = %+v, %v, %v", found.ID, ok, err)
	}
	if _, ok, err := s.OccurrenceByFire(ctx, "job-1", fire.Add(time.Hour)); err != nil || ok {
		t.Errorf("missing fire lookup = %v, %v", ok, err)
	}

	if err := s.SettleOccurrence(ctx, "cron-job-1-1", OccurrenceCompleted, "turn-1", "", "", now); err != nil {
		t.Fatalf("SettleOccurrence(): %v", err)
	}
	if err := s.SettleOccurrence(ctx, "cron-job-1-1", OccurrenceFailed, "", "", "x", now); err == nil {
		t.Error("double settle accepted")
	}
	history, err := s.Occurrences(ctx, "job-1", 10)
	if err != nil {
		t.Fatalf("Occurrences(): %v", err)
	}
	if len(history) != 1 || history[0].State != OccurrenceCompleted || history[0].TurnID != "turn-1" {
		t.Errorf("history = %+v", history)
	}
}

func TestScheduleStore_RejectsInvalidRows(t *testing.T) {
	db := newTestDB(t)
	s := NewScheduleStore(db)
	ctx := t.Context()

	badPolicy := testScheduledJob("job-1")
	badPolicy.OverlapPolicy = "parallel"
	if err := s.InsertJob(ctx, badPolicy); err == nil {
		t.Error("bad policy accepted")
	}
	badState := testScheduledJob("job-2")
	badState.State = "flying"
	if err := s.InsertJob(ctx, badState); err == nil {
		t.Error("bad state accepted")
	}
	if err := s.InsertJob(ctx, nil); err == nil {
		t.Error("nil job accepted")
	}
	if _, err := s.EditJob(ctx, "missing", &JobEdit{
		Name: "x", CronExpression: "0 21 * * *", Timezone: "UTC", Prompt: "p",
		OriginChannel: "discord", OriginDestination: "default",
		OverlapPolicy: ScheduleOverlapSkip, ExpectedVersion: 1,
	}, time.Now().UTC()); err == nil {
		t.Error("edit of missing job accepted")
	}
}

func TestScheduleStore_SchemaVersionTwelve(t *testing.T) {
	db := newTestDB(t)
	applied, latest, err := SchemaVersions(t.Context(), db)
	if err != nil {
		t.Fatalf("SchemaVersions(): %v", err)
	}
	if latest != 12 || applied != 12 {
		t.Errorf("schema = applied %d latest %d, want 12", applied, latest)
	}
}
