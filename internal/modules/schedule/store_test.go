package schedule

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/store/history"
	"github.com/anggasct/aura/internal/store/storetest"
)

func occurrenceIDs(items []Occurrence) []string {
	ids := make([]string, 0, len(items))
	for i := range items {
		ids = append(ids, items[i].ID)
	}
	return ids
}

func testScheduledJob(id string) *Job {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	return &Job{
		ID: id, Name: "nightly", CronExpression: "0 21 * * *", Timezone: "UTC",
		Prompt: "summarize", OriginChannel: "discord", OriginDestination: "default",
		OverlapPolicy: OverlapSkip, CatchUpGraceSeconds: 900,
		State: JobActive, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
}

func mustNewSQLiteStore(t *testing.T) *SQLiteStore {
	t.Helper()
	db := storetest.OpenMigrated(t)
	s, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore(): %v", err)
	}
	return s
}

func TestNewSQLiteStoreRefusesNilDB(t *testing.T) {
	if _, err := NewSQLiteStore(nil); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("NewSQLiteStore(nil) = %v, want invalid_argument", err)
	}
}

func TestScheduleStore_JobLifecycle(t *testing.T) {
	s := mustNewSQLiteStore(t)
	ctx := t.Context()

	if err := s.InsertJob(ctx, testScheduledJob("job-1")); err != nil {
		t.Fatalf("InsertJob(): %v", err)
	}
	if err := s.InsertJob(ctx, testScheduledJob("job-1")); err == nil {
		t.Fatal("duplicate job accepted")
	} else if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrorCodeOccurrenceConflict {
		t.Errorf("duplicate code = %v, %v", code, ok)
	}
	job, found, err := s.Job(ctx, "job-1")
	if err != nil || !found {
		t.Fatalf("Job(): found=%v err=%v", found, err)
	}
	if job.Name != "nightly" || job.Version != 1 || job.State != JobActive {
		t.Errorf("job mismatch: %+v", job)
	}
	if _, found, err := s.Job(ctx, "missing"); err != nil || found {
		t.Errorf("missing job = found=%v err=%v, want false, nil", found, err)
	}

	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	edited, err := s.EditJob(ctx, "job-1", &JobEdit{
		Name: "renamed", CronExpression: "30 22 * * *", Timezone: "UTC",
		Prompt: "summarize", OriginChannel: "discord", OriginDestination: "default",
		OverlapPolicy: OverlapQueueOne, CatchUpGraceSeconds: 300,
	}, 1, now)
	if err != nil {
		t.Fatalf("EditJob(): %v", err)
	}
	if edited.Version != 2 || edited.Name != "renamed" {
		t.Errorf("edited job = %+v", edited)
	}
	if _, err := s.EditJob(ctx, "job-1", &JobEdit{
		Name: "stale", CronExpression: "0 21 * * *", Timezone: "UTC",
		Prompt: "summarize", OriginChannel: "discord", OriginDestination: "default",
		OverlapPolicy: OverlapSkip, CatchUpGraceSeconds: 900,
	}, 1, now); !fault.HasCode(err, ErrorCodeOccurrenceConflict) {
		t.Errorf("stale edit err = %v, want conflict", err)
	}

	if err := s.SetJobState(ctx, "job-1", JobPaused, now); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := s.SetJobState(ctx, "job-1", JobActive, now); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if err := s.SetJobState(ctx, "job-1", JobDeleted, now); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.SetJobState(ctx, "job-1", JobActive, now); !fault.HasCode(err, ErrorCodeOccurrenceConflict) {
		t.Errorf("resurrect err = %v, want conflict", err)
	}
	if err := s.SetJobState(ctx, "missing", JobPaused, now); !fault.HasCode(err, ErrorCodeJobNotFound) {
		t.Errorf("missing state err = %v, want not found", err)
	}
	if err := s.SetJobState(ctx, "job-1", "flying", now); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Errorf("invalid state err = %v, want invalid_argument", err)
	}
}

func TestScheduleStore_OccurrenceFireAndSettle(t *testing.T) {
	s := mustNewSQLiteStore(t)
	ctx := t.Context()

	if err := s.InsertJob(ctx, testScheduledJob("job-1")); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	fire := time.Date(2026, 9, 12, 21, 0, 0, 0, time.UTC)
	now := time.Date(2026, 9, 12, 21, 0, 5, 0, time.UTC)
	occurrence := &Occurrence{
		ID: "cron-job-1-1", JobID: "job-1", JobVersion: 1, ScheduledForUTC: fire,
		State: OccurrenceFired, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.RecordFire(ctx, occurrence); err != nil {
		t.Fatalf("RecordFire(): %v", err)
	}
	if err := s.RecordFire(ctx, occurrence); err == nil {
		t.Fatal("duplicate fire accepted")
	} else if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrorCodeOccurrenceConflict {
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
	occurrences, err := s.Occurrences(ctx, "job-1", 10)
	if err != nil {
		t.Fatalf("Occurrences(): %v", err)
	}
	if len(occurrences) != 1 || occurrences[0].State != OccurrenceCompleted || occurrences[0].TurnID != "turn-1" {
		t.Errorf("history = %+v", occurrences)
	}
}

func TestScheduleStore_RejectsInvalidRows(t *testing.T) {
	s := mustNewSQLiteStore(t)
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
		OverlapPolicy: OverlapSkip,
	}, 1, time.Now().UTC()); !fault.HasCode(err, ErrorCodeJobNotFound) {
		t.Errorf("edit of missing job err = %v, want job not found", err)
	}
}

func TestScheduleStore_FiredOccurrencesAndPrune(t *testing.T) {
	s := mustNewSQLiteStore(t)
	ctx := t.Context()

	for _, id := range []string{"job-1", "job-2"} {
		if err := s.InsertJob(ctx, testScheduledJob(id)); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	fire := time.Date(2026, 9, 12, 21, 0, 0, 0, time.UTC)
	now := fire.Add(3 * time.Hour)
	seeded := []*Occurrence{
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
	terminal := &Occurrence{
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
	s := mustNewSQLiteStore(t)
	ctx := t.Context()

	if err := s.InsertJob(ctx, testScheduledJob("job-1")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	paused := testScheduledJob("job-2")
	paused.State = JobPaused
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
	db := storetest.OpenMigrated(t)
	applied, latest, err := sqlite.SchemaVersions(t.Context(), db, history.Migrations())
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
	s := mustNewSQLiteStore(t)
	ctx := t.Context()

	fire := time.Date(2026, 9, 12, 21, 0, 0, 0, time.UTC)
	now := time.Date(2026, 9, 12, 21, 0, 5, 0, time.UTC)
	err := s.RecordFire(ctx, &Occurrence{
		ID: "cron-missing-1", JobID: "missing-job", JobVersion: 1, ScheduledForUTC: fire,
		State: OccurrenceFired, CreatedAt: now, UpdatedAt: now,
	})
	if err == nil {
		t.Fatal("fire for missing job accepted")
	}
	if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrorCodeJobNotFound {
		t.Errorf("missing-job code = %v, %v; want %v", code, ok, ErrorCodeJobNotFound)
	}
}

func TestScheduleStoreJoinsCallersTransaction(t *testing.T) {
	db := storetest.OpenMigrated(t)
	s, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore(): %v", err)
	}
	ctx := t.Context()
	rollbackErr := errors.New("abort outer transaction")

	err = sqlite.RunInTx(ctx, db, func(txCtx context.Context) error {
		if err := s.InsertJob(txCtx, testScheduledJob("job-tx")); err != nil {
			return err
		}
		if _, found, err := s.Job(txCtx, "job-tx"); err != nil || !found {
			t.Fatalf("Job inside tx = found=%v err=%v", found, err)
		}
		return rollbackErr
	})
	if !errors.Is(err, rollbackErr) {
		t.Fatalf("RunInTx err = %v, want %v", err, rollbackErr)
	}
	if _, found, err := s.Job(ctx, "job-tx"); err != nil || found {
		t.Fatalf("job survived rolled-back tx: found=%v err=%v", found, err)
	}
}
