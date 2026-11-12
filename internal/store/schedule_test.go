package store

import (
	"slices"
	"testing"
	"time"
)

func occurrenceIDs(items []ScheduledOccurrence) []string {
	ids := make([]string, 0, len(items))
	for i := range items {
		ids = append(ids, items[i].ID)
	}
	return ids
}

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

func TestScheduleStore_FiredOccurrencesAndPrune(t *testing.T) {
	db := newTestDB(t)
	s := NewScheduleStore(db)
	ctx := t.Context()

	for _, id := range []string{"job-1", "job-2"} {
		if err := s.InsertJob(ctx, testScheduledJob(id)); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	fire := time.Date(2026, 9, 12, 21, 0, 0, 0, time.UTC)
	now := fire.Add(3 * time.Hour)
	seeded := []*ScheduledOccurrence{
		{ID: "cron-newest", JobID: "job-1", JobVersion: 1, ScheduledForUTC: fire.Add(2 * time.Hour), State: OccurrenceFired, CreatedAt: now, UpdatedAt: now},
		{ID: "cron-attached", JobID: "job-1", JobVersion: 1, ScheduledForUTC: fire.Add(time.Hour), State: OccurrenceFired, CreatedAt: now, UpdatedAt: now},
		{ID: "cron-oldest", JobID: "job-2", JobVersion: 1, ScheduledForUTC: fire, State: OccurrenceFired, CreatedAt: now, UpdatedAt: now},
		{ID: "cron-settled", JobID: "job-2", JobVersion: 1, ScheduledForUTC: fire.Add(-time.Hour), State: OccurrenceCompleted, CreatedAt: now, UpdatedAt: now},
	}
	for _, occurrence := range seeded {
		if err := s.RecordFire(ctx, occurrence); err != nil {
			t.Fatalf("seed %s: %v", occurrence.ID, err)
		}
	}
	if err := s.AttachTurn(ctx, "cron-attached", "turn-1", now); err != nil {
		t.Fatalf("attach: %v", err)
	}
	fired, err := s.FiredOccurrences(ctx, 10)
	if err != nil {
		t.Fatalf("FiredOccurrences(): %v", err)
	}
	if got := occurrenceIDs(fired); !slices.Equal(got, []string{"cron-oldest", "cron-attached", "cron-newest"}) {
		t.Errorf("FiredOccurrences() = %v, want the fired rows of every job, oldest first", got)
	}
	limited, err := s.FiredOccurrences(ctx, 2)
	if err != nil {
		t.Fatalf("FiredOccurrences(limit 2): %v", err)
	}
	if got := occurrenceIDs(limited); !slices.Equal(got, []string{"cron-oldest", "cron-attached"}) {
		t.Errorf("FiredOccurrences(limit 2) = %v, want the two oldest", got)
	}
	if _, err := s.FiredOccurrences(ctx, 0); err == nil {
		t.Error("non-positive fired limit accepted")
	}

	old := now.Add(-1000 * time.Hour)
	terminal := &ScheduledOccurrence{
		ID: "cron-old", JobID: "job-1", JobVersion: 1, ScheduledForUTC: old,
		State: OccurrenceCompleted, CreatedAt: old, UpdatedAt: old,
	}
	if err := s.RecordFire(ctx, terminal); err != nil {
		t.Fatalf("seed terminal: %v", err)
	}
	pruned, err := s.PruneOccurrences(ctx, "job-1", now.Add(-720*time.Hour), 100)
	if err != nil {
		t.Fatalf("PruneOccurrences(): %v", err)
	}
	if pruned != 1 {
		t.Errorf("pruned = %d, want 1 (active rows never pruned)", pruned)
	}
	if _, found, err := s.OccurrenceByFire(ctx, "job-1", old); err != nil || found {
		t.Errorf("pruned row lookup = %v, %v; want not found", found, err)
	}
	if pruned, err := s.PruneOccurrences(ctx, "job-1", now, 100); err != nil || pruned != 0 {
		t.Errorf("fresh prune = %d, %v; want 0", pruned, err)
	}
	if _, err := s.PruneOccurrences(ctx, "", now, 100); err == nil {
		t.Error("empty alias prune accepted")
	}
	if _, err := s.PruneOccurrences(ctx, "job-1", now, 0); err == nil {
		t.Error("non-positive prune limit accepted")
	}
}

func TestScheduleStore_ActiveJobsAndListJobs(t *testing.T) {
	db := newTestDB(t)
	s := NewScheduleStore(db)
	ctx := t.Context()

	if err := s.InsertJob(ctx, testScheduledJob("job-1")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	paused := testScheduledJob("job-2")
	paused.State = ScheduleJobPaused
	if err := s.InsertJob(ctx, paused); err != nil {
		t.Fatalf("seed paused: %v", err)
	}
	active, err := s.ActiveJobs(ctx, 10)
	if err != nil {
		t.Fatalf("ActiveJobs(): %v", err)
	}
	if len(active) != 1 || active[0].ID != "job-1" {
		t.Errorf("active jobs = %+v", active)
	}
	all, err := s.ListJobs(ctx, 10)
	if err != nil {
		t.Fatalf("ListJobs(): %v", err)
	}
	if len(all) != 2 {
		t.Errorf("all jobs = %d, want 2", len(all))
	}
	if _, err := s.ListJobs(ctx, 0); err == nil {
		t.Error("non-positive limit accepted")
	}
}

func TestScheduleStore_SchemaAppliesCleanly(t *testing.T) {
	db := newTestDB(t)
	applied, latest, err := SchemaVersions(t.Context(), db)
	if err != nil {
		t.Fatalf("SchemaVersions(): %v", err)
	}
	if applied != latest {
		t.Errorf("applied schema = %d, latest = %d; want fully migrated", applied, latest)
	}
	var ddl string
	if err := db.QueryRowContext(t.Context(), `SELECT sql FROM sqlite_master WHERE name = 'scheduled_occurrence'`).Scan(&ddl); err != nil {
		t.Fatalf("scheduled_occurrence DDL: %v", err)
	}
}

func TestScheduleStore_RecordFireMissingJob(t *testing.T) {
	db := newTestDB(t)
	s := NewScheduleStore(db)
	ctx := t.Context()

	fire := time.Date(2026, 9, 12, 21, 0, 0, 0, time.UTC)
	now := time.Date(2026, 9, 12, 21, 0, 5, 0, time.UTC)
	err := s.RecordFire(ctx, &ScheduledOccurrence{
		ID: "cron-missing-1", JobID: "missing-job", JobVersion: 1, ScheduledForUTC: fire,
		State: OccurrenceFired, CreatedAt: now, UpdatedAt: now,
	})
	if err == nil {
		t.Fatal("fire for missing job accepted")
	}
	if code, ok := CodeOf(err); !ok || code != ErrorCodeScheduleNotFound {
		t.Errorf("missing-job code = %v, %v; want %v", code, ok, ErrorCodeScheduleNotFound)
	}
}
