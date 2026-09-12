package scheduler

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"time"
)

const (
	JobActive  = "active"
	JobPaused  = "paused"
	JobDeleted = "deleted"
)

const (
	OverlapSkip     = "skip"
	OverlapQueueOne = "queue_one"
)

const (
	OccurrenceFired          = "fired"
	OccurrenceCompleted      = "completed"
	OccurrenceFailed         = "failed"
	OccurrenceExpired        = "expired"
	OccurrenceSkippedOverlap = "skipped_overlap"
	OccurrenceCancelled      = "cancelled"
)

const maxNameRunes = 128

const maxPromptBytes = 1 << 16

const maxOriginRunes = 256

type Job struct {
	ID                  string
	Name                string
	CronExpression      string
	Timezone            string
	Prompt              string
	OriginChannel       string
	OriginDestination   string
	OverlapPolicy       string
	CatchUpGraceSeconds int64
	State               string
	Version             int64
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

type JobSpec struct {
	Name                string
	CronExpression      string
	Timezone            string
	Prompt              string
	OriginChannel       string
	OriginDestination   string
	OverlapPolicy       string
	CatchUpGraceSeconds int64
}

type JobEdit struct {
	Name                string
	CronExpression      string
	Timezone            string
	Prompt              string
	OriginChannel       string
	OriginDestination   string
	OverlapPolicy       string
	CatchUpGraceSeconds int64
}

type Occurrence struct {
	ID              string
	JobID           string
	JobVersion      int64
	ScheduledForUTC time.Time
	State           string
	TurnID          string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type JobStore interface {
	InsertJob(ctx context.Context, job *Job) error
	Job(ctx context.Context, id string) (Job, bool, error)
	EditJob(ctx context.Context, id string, edit *JobEdit, expectedVersion int64, now time.Time) (Job, error)
	SetJobState(ctx context.Context, id, state string, now time.Time) error
	RecordFire(ctx context.Context, occurrence *Occurrence) error
	OccurrenceByFire(ctx context.Context, jobID string, scheduledFor time.Time) (Occurrence, bool, error)
}

type Service struct {
	jobs JobStore
}

func NewService(jobs JobStore) (*Service, error) {
	if jobs == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "job store must not be nil")
	}
	return &Service{jobs: jobs}, nil
}

func newJobID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "job_" + hex.EncodeToString(raw[:]), nil
}

func validateJobSpec(spec *JobSpec) error {
	if spec == nil {
		return Errorf(ErrorCodeInvalidArgument, "job spec must not be nil")
	}
	if strings.TrimSpace(spec.Name) == "" || len([]rune(spec.Name)) > maxNameRunes {
		return Errorf(ErrorCodeInvalidArgument, "job name is not valid")
	}
	if _, err := ParseExpression(spec.CronExpression); err != nil {
		return err
	}
	if _, err := time.LoadLocation(spec.Timezone); err != nil {
		return Errorf(ErrorCodeTimezoneInvalid, "timezone is not a valid IANA name")
	}
	if strings.TrimSpace(spec.Prompt) == "" || len(spec.Prompt) > maxPromptBytes {
		return Errorf(ErrorCodeInvalidArgument, "job prompt is not within bounds")
	}
	if strings.TrimSpace(spec.OriginChannel) == "" || len([]rune(spec.OriginChannel)) > maxOriginRunes {
		return Errorf(ErrorCodeInvalidArgument, "job origin channel is not valid")
	}
	if strings.TrimSpace(spec.OriginDestination) == "" || len([]rune(spec.OriginDestination)) > maxOriginRunes {
		return Errorf(ErrorCodeInvalidArgument, "job origin destination is not valid")
	}
	if spec.OverlapPolicy != OverlapSkip && spec.OverlapPolicy != OverlapQueueOne {
		return Errorf(ErrorCodeInvalidArgument, "overlap policy is not valid")
	}
	if spec.CatchUpGraceSeconds < 0 {
		return Errorf(ErrorCodeInvalidArgument, "catch-up grace must not be negative")
	}
	return nil
}

func (s *Service) Create(ctx context.Context, spec *JobSpec) (Job, error) {
	if ctx == nil {
		return Job{}, Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if err := validateJobSpec(spec); err != nil {
		return Job{}, err
	}
	id, err := newJobID()
	if err != nil {
		return Job{}, Errorf(ErrorCodeUnavailable, "job identity is unavailable")
	}
	now := time.Now().UTC()
	job := &Job{
		ID: id, Name: spec.Name, CronExpression: spec.CronExpression, Timezone: spec.Timezone,
		Prompt: spec.Prompt, OriginChannel: spec.OriginChannel, OriginDestination: spec.OriginDestination,
		OverlapPolicy: spec.OverlapPolicy, CatchUpGraceSeconds: spec.CatchUpGraceSeconds,
		State: JobActive, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.jobs.InsertJob(ctx, job); err != nil {
		return Job{}, err
	}
	return *job, nil
}

func (s *Service) Get(ctx context.Context, id string) (Job, error) {
	if ctx == nil {
		return Job{}, Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	stored, found, err := s.jobs.Job(ctx, id)
	if err != nil {
		return Job{}, err
	}
	if !found {
		return Job{}, Errorf(ErrorCodeJobNotFound, "schedule job not found")
	}
	return stored, nil
}

func (s *Service) Edit(ctx context.Context, id string, edit *JobEdit) (Job, error) {
	if ctx == nil {
		return Job{}, Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if edit == nil {
		return Job{}, Errorf(ErrorCodeInvalidArgument, "job edit must not be nil")
	}
	if err := validateJobSpec(&JobSpec{
		Name: edit.Name, CronExpression: edit.CronExpression, Timezone: edit.Timezone,
		Prompt: edit.Prompt, OriginChannel: edit.OriginChannel, OriginDestination: edit.OriginDestination,
		OverlapPolicy: edit.OverlapPolicy, CatchUpGraceSeconds: edit.CatchUpGraceSeconds,
	}); err != nil {
		return Job{}, err
	}
	current, found, err := s.jobs.Job(ctx, id)
	if err != nil {
		return Job{}, err
	}
	if !found {
		return Job{}, Errorf(ErrorCodeJobNotFound, "schedule job not found")
	}
	updated, err := s.jobs.EditJob(ctx, id, &JobEdit{
		Name: edit.Name, CronExpression: edit.CronExpression, Timezone: edit.Timezone,
		Prompt: edit.Prompt, OriginChannel: edit.OriginChannel, OriginDestination: edit.OriginDestination,
		OverlapPolicy: edit.OverlapPolicy, CatchUpGraceSeconds: edit.CatchUpGraceSeconds,
	}, current.Version, time.Now().UTC())
	if err != nil {
		return Job{}, err
	}
	return updated, nil
}

func (s *Service) SetState(ctx context.Context, id, state string) error {
	if ctx == nil {
		return Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if state != JobActive && state != JobPaused && state != JobDeleted {
		return Errorf(ErrorCodeJobStateInvalid, "job state is not valid")
	}
	return s.jobs.SetJobState(ctx, id, state, time.Now().UTC())
}

func (s *Service) NextFire(ctx context.Context, id string, after time.Time) (time.Time, error) {
	if ctx == nil {
		return time.Time{}, Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	stored, found, err := s.jobs.Job(ctx, id)
	if err != nil {
		return time.Time{}, err
	}
	if !found {
		return time.Time{}, Errorf(ErrorCodeJobNotFound, "schedule job not found")
	}
	schedule, err := ParseExpression(stored.CronExpression)
	if err != nil {
		return time.Time{}, err
	}
	location, err := time.LoadLocation(stored.Timezone)
	if err != nil {
		return time.Time{}, Errorf(ErrorCodeTimezoneInvalid, "timezone is not a valid IANA name")
	}
	return NextFire(&schedule, location, after)
}

func occurrenceID(jobID string, fire time.Time) string {
	return "cron-" + jobID + "-" + strings.ReplaceAll(fire.UTC().Format("20060102T150405"), ":", "")
}

func newManualOccurrence(jobID string, fire time.Time, version int64, now time.Time) (Occurrence, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return Occurrence{}, Errorf(ErrorCodeUnavailable, "manual occurrence identity is unavailable")
	}
	nanos := int64(binary.BigEndian.Uint32(raw[:4])%999999999) + 1
	scheduledFor := fire.UTC().Add(time.Duration(nanos) * time.Nanosecond)
	id := occurrenceID(jobID, fire) + "-manual-" + hex.EncodeToString(raw[4:])
	return Occurrence{
		ID: id, JobID: jobID, JobVersion: version, ScheduledForUTC: scheduledFor,
		State: OccurrenceFired, CreatedAt: now, UpdatedAt: now,
	}, nil
}

func (s *Service) RecordFire(ctx context.Context, jobID string, fire time.Time, version int64) (Occurrence, bool, error) {
	if ctx == nil {
		return Occurrence{}, false, Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return Occurrence{}, false, err
	}
	now := time.Now().UTC()
	id := occurrenceID(jobID, fire)
	err := s.jobs.RecordFire(ctx, &Occurrence{
		ID: id, JobID: jobID, JobVersion: version, ScheduledForUTC: fire.UTC(),
		State: OccurrenceFired, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		if existing, found, findErr := s.jobs.OccurrenceByFire(ctx, jobID, fire.UTC()); findErr == nil && found {
			return existing, true, nil
		}
		return Occurrence{}, false, err
	}
	return Occurrence{
		ID: id, JobID: jobID, JobVersion: version, ScheduledForUTC: fire.UTC(),
		State: OccurrenceFired, CreatedAt: now, UpdatedAt: now,
	}, false, nil
}
func (s *Service) RunNow(ctx context.Context, id string, now time.Time) (Occurrence, error) {
	if ctx == nil {
		return Occurrence{}, Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return Occurrence{}, err
	}
	stored, found, err := s.jobs.Job(ctx, id)
	if err != nil {
		return Occurrence{}, err
	}
	if !found {
		return Occurrence{}, Errorf(ErrorCodeJobNotFound, "schedule job not found")
	}
	if stored.State == JobDeleted {
		return Occurrence{}, Errorf(ErrorCodeJobStateInvalid, "deleted jobs do not fire")
	}
	base := now.UTC().Truncate(time.Second)
	for range 5 {
		candidate, err := newManualOccurrence(id, base, stored.Version, time.Now().UTC())
		if err != nil {
			return Occurrence{}, err
		}
		if err := s.jobs.RecordFire(ctx, &candidate); err != nil {
			if code, ok := CodeOf(err); ok && code == ErrorCodeOccurrenceConflict {
				continue
			}
			return Occurrence{}, err
		}
		return candidate, nil
	}
	return Occurrence{}, Errorf(ErrorCodeUnavailable, "manual occurrence identity is unavailable")
}
