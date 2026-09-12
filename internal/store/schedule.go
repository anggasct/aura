package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	ScheduleJobActive  = "active"
	ScheduleJobPaused  = "paused"
	ScheduleJobDeleted = "deleted"
)

const (
	ScheduleOverlapSkip     = "skip"
	ScheduleOverlapQueueOne = "queue_one"
)

const (
	OccurrenceFired          = "fired"
	OccurrenceCompleted      = "completed"
	OccurrenceFailed         = "failed"
	OccurrenceExpired        = "expired"
	OccurrenceSkippedOverlap = "skipped_overlap"
	OccurrenceCancelled      = "cancelled"
)

type ScheduledJob struct {
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

type JobEdit struct {
	Name                string
	CronExpression      string
	Timezone            string
	Prompt              string
	OriginChannel       string
	OriginDestination   string
	OverlapPolicy       string
	CatchUpGraceSeconds int64
	ExpectedVersion     int64
}

type ScheduledOccurrence struct {
	ID              string
	JobID           string
	JobVersion      int64
	ScheduledForUTC time.Time
	State           string
	TurnID          string
	ResultEventID   string
	SafeErrorCode   string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type ScheduleStore interface {
	InsertJob(ctx context.Context, job *ScheduledJob) error
	Job(ctx context.Context, id string) (ScheduledJob, error)
	EditJob(ctx context.Context, id string, edit *JobEdit, now time.Time) (ScheduledJob, error)
	SetJobState(ctx context.Context, id, state string, now time.Time) error
	RecordFire(ctx context.Context, occurrence *ScheduledOccurrence) error
	OccurrenceByFire(ctx context.Context, jobID string, scheduledFor time.Time) (ScheduledOccurrence, bool, error)
	ActiveJobs(ctx context.Context, limit int) ([]ScheduledJob, error)
	PendingFire(ctx context.Context, jobID string) (ScheduledOccurrence, bool, error)
	PruneOccurrences(ctx context.Context, jobID string, before time.Time, limit int) (int, error)
	ListJobs(ctx context.Context, limit int) ([]ScheduledJob, error)
	ActiveOccurrence(ctx context.Context, jobID string) (ScheduledOccurrence, bool, error)
	AttachTurn(ctx context.Context, id, turnID string, now time.Time) error
	SettleOccurrence(ctx context.Context, id, state, turnID, resultEventID, safeErrorCode string, now time.Time) error
	Occurrences(ctx context.Context, jobID string, limit int) ([]ScheduledOccurrence, error)
}

type sqliteScheduleStore struct {
	db *sql.DB
}

func NewScheduleStore(db *sql.DB) ScheduleStore {
	return &sqliteScheduleStore{db: db}
}

func validJobState(state string) bool {
	switch state {
	case ScheduleJobActive, ScheduleJobPaused, ScheduleJobDeleted:
		return true
	default:
		return false
	}
}

func validOverlapPolicy(policy string) bool {
	return policy == ScheduleOverlapSkip || policy == ScheduleOverlapQueueOne
}

func validOccurrenceState(state string) bool {
	switch state {
	case OccurrenceFired, OccurrenceCompleted, OccurrenceFailed,
		OccurrenceExpired, OccurrenceSkippedOverlap, OccurrenceCancelled:
		return true
	default:
		return false
	}
}

func validateScheduledJob(job *ScheduledJob) error {
	if job == nil {
		return errNilArgument("job")
	}
	if job.ID == "" || job.Name == "" || job.CronExpression == "" || job.Timezone == "" {
		return Errorf(ErrorCodeInvalidArgument, "schedule job identity, name, expression, and timezone must not be empty")
	}
	if job.Prompt == "" || job.OriginChannel == "" || job.OriginDestination == "" {
		return Errorf(ErrorCodeInvalidArgument, "schedule prompt and origin must not be empty")
	}
	if !validOverlapPolicy(job.OverlapPolicy) {
		return Errorf(ErrorCodeScheduleInvalid, "schedule overlap policy is not valid")
	}
	if job.CatchUpGraceSeconds < 0 {
		return Errorf(ErrorCodeScheduleInvalid, "schedule catch-up grace must not be negative")
	}
	if !validJobState(job.State) {
		return Errorf(ErrorCodeScheduleInvalid, "schedule job state is not valid")
	}
	if job.Version <= 0 {
		return Errorf(ErrorCodeScheduleInvalid, "schedule job version must be positive")
	}
	return nil
}

func (s *sqliteScheduleStore) InsertJob(ctx context.Context, job *ScheduledJob) error {
	if s.db == nil {
		return errNilArgument("db")
	}
	if err := validateScheduledJob(job); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO scheduled_job (
			id, name, cron_expression, timezone, prompt, origin_channel, origin_destination,
			overlap_policy, catch_up_grace_seconds, state, version, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		job.ID, job.Name, job.CronExpression, job.Timezone, job.Prompt,
		job.OriginChannel, job.OriginDestination, job.OverlapPolicy, job.CatchUpGraceSeconds,
		job.State, job.Version, formatTime(job.CreatedAt.UTC()), formatTime(job.UpdatedAt.UTC()),
	)
	if err != nil {
		if isConstraintUnique(err) {
			return &Error{Code: ErrorCodeScheduleConflict, Detail: fmt.Sprintf("schedule job %q already exists", job.ID)}
		}
		return classifyBusy(fmt.Errorf("insert schedule job: %w", err))
	}
	return nil
}

func (s *sqliteScheduleStore) Job(ctx context.Context, id string) (ScheduledJob, error) {
	if s.db == nil {
		return ScheduledJob{}, errNilArgument("db")
	}
	job, found, err := scanOneScheduledJob(s.db.QueryRowContext(ctx,
		`SELECT id, name, cron_expression, timezone, prompt, origin_channel, origin_destination,
		overlap_policy, catch_up_grace_seconds, state, version, created_at, updated_at
		FROM scheduled_job WHERE id = ?`, id))
	if err != nil {
		return ScheduledJob{}, err
	}
	if !found {
		return ScheduledJob{}, &Error{Code: ErrorCodeScheduleNotFound, Detail: "schedule job not found"}
	}
	return job, nil
}

func (s *sqliteScheduleStore) EditJob(ctx context.Context, id string, edit *JobEdit, now time.Time) (ScheduledJob, error) {
	if s.db == nil {
		return ScheduledJob{}, errNilArgument("db")
	}
	if edit == nil {
		return ScheduledJob{}, errNilArgument("edit")
	}
	candidate := &ScheduledJob{
		ID: id, Name: edit.Name, CronExpression: edit.CronExpression, Timezone: edit.Timezone,
		Prompt: edit.Prompt, OriginChannel: edit.OriginChannel, OriginDestination: edit.OriginDestination,
		OverlapPolicy: edit.OverlapPolicy, CatchUpGraceSeconds: edit.CatchUpGraceSeconds,
		State: ScheduleJobActive, Version: edit.ExpectedVersion + 1,
	}
	if err := validateScheduledJob(candidate); err != nil {
		return ScheduledJob{}, err
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE scheduled_job SET name = ?, cron_expression = ?, timezone = ?, prompt = ?,
		origin_channel = ?, origin_destination = ?, overlap_policy = ?, catch_up_grace_seconds = ?,
		version = ?, updated_at = ? WHERE id = ? AND version = ? AND state != ?`,
		edit.Name, edit.CronExpression, edit.Timezone, edit.Prompt,
		edit.OriginChannel, edit.OriginDestination, edit.OverlapPolicy, edit.CatchUpGraceSeconds,
		edit.ExpectedVersion+1, formatTime(now.UTC()), id, edit.ExpectedVersion, ScheduleJobDeleted)
	if err != nil {
		return ScheduledJob{}, classifyBusy(fmt.Errorf("edit schedule job: %w", err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return ScheduledJob{}, classifyBusy(fmt.Errorf("edit schedule job: %w", err))
	}
	if affected == 0 {
		if _, findErr := s.Job(ctx, id); findErr != nil {
			return ScheduledJob{}, findErr
		}
		return ScheduledJob{}, &Error{Code: ErrorCodeScheduleConflict, Detail: "schedule job changed underneath the edit"}
	}
	return s.Job(ctx, id)
}

func (s *sqliteScheduleStore) SetJobState(ctx context.Context, id, state string, now time.Time) error {
	if s.db == nil {
		return errNilArgument("db")
	}
	if !validJobState(state) {
		return Errorf(ErrorCodeInvalidArgument, "schedule job state is not valid")
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE scheduled_job SET state = ?, updated_at = ? WHERE id = ? AND state != ?`,
		state, formatTime(now.UTC()), id, ScheduleJobDeleted)
	if err != nil {
		return classifyBusy(fmt.Errorf("set schedule job state: %w", err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return classifyBusy(fmt.Errorf("set schedule job state: %w", err))
	}
	if affected == 0 {
		if _, findErr := s.Job(ctx, id); findErr != nil {
			return findErr
		}
		return &Error{Code: ErrorCodeScheduleConflict, Detail: "schedule job state transition is not allowed"}
	}
	return nil
}

func (s *sqliteScheduleStore) RecordFire(ctx context.Context, occurrence *ScheduledOccurrence) error {
	if s.db == nil {
		return errNilArgument("db")
	}
	if occurrence == nil {
		return errNilArgument("occurrence")
	}
	if occurrence.ID == "" || occurrence.JobID == "" {
		return Errorf(ErrorCodeInvalidArgument, "occurrence identity must not be empty")
	}
	if !validOccurrenceState(occurrence.State) {
		return Errorf(ErrorCodeScheduleInvalid, "occurrence state is not valid")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO scheduled_occurrence (
			id, job_id, job_version, scheduled_for_utc, state, turn_id,
			result_event_id, safe_error_code, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		occurrence.ID, occurrence.JobID, occurrence.JobVersion,
		formatTime(occurrence.ScheduledForUTC.UTC()), occurrence.State,
		nullText(occurrence.TurnID), nullText(occurrence.ResultEventID), nullText(occurrence.SafeErrorCode),
		formatTime(occurrence.CreatedAt.UTC()), formatTime(occurrence.UpdatedAt.UTC()),
	)
	if err != nil {
		if isConstraintUnique(err) {
			return &Error{Code: ErrorCodeScheduleConflict, Detail: "occurrence already recorded for this fire"}
		}
		if isConstraintForeignKey(err) {
			return &Error{Code: ErrorCodeScheduleNotFound, Detail: fmt.Sprintf("schedule job %q does not exist", occurrence.JobID)}
		}
		return classifyBusy(fmt.Errorf("record occurrence fire: %w", err))
	}
	return nil
}

func (s *sqliteScheduleStore) SettleOccurrence(ctx context.Context, id, state, turnID, resultEventID, safeErrorCode string, now time.Time) error {
	if s.db == nil {
		return errNilArgument("db")
	}
	if id == "" {
		return Errorf(ErrorCodeInvalidArgument, "occurrence id must not be empty")
	}
	if state != OccurrenceCompleted && state != OccurrenceFailed && state != OccurrenceExpired &&
		state != OccurrenceSkippedOverlap && state != OccurrenceCancelled {
		return Errorf(ErrorCodeInvalidArgument, "settle state is not terminal")
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE scheduled_occurrence SET state = ?, turn_id = COALESCE(NULLIF(turn_id, ''), ?),
		result_event_id = ?, safe_error_code = ?, updated_at = ?
		WHERE id = ? AND state = ?`,
		state, nullText(turnID), nullText(resultEventID), nullText(safeErrorCode),
		formatTime(now.UTC()), id, OccurrenceFired)
	if err != nil {
		return classifyBusy(fmt.Errorf("settle occurrence: %w", err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return classifyBusy(fmt.Errorf("settle occurrence: %w", err))
	}
	if affected == 0 {
		if err := s.requireOccurrence(ctx, id); err != nil {
			return err
		}
		return &Error{Code: ErrorCodeScheduleConflict, Detail: "occurrence is already terminal"}
	}
	return nil
}

func (s *sqliteScheduleStore) Occurrences(ctx context.Context, jobID string, limit int) ([]ScheduledOccurrence, error) {
	if s.db == nil {
		return nil, errNilArgument("db")
	}
	if jobID == "" {
		return nil, Errorf(ErrorCodeInvalidArgument, "job id must not be empty")
	}
	if limit <= 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "list limit must be positive")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, job_id, job_version, scheduled_for_utc, state, turn_id,
		result_event_id, safe_error_code, created_at, updated_at
		FROM scheduled_occurrence WHERE job_id = ? ORDER BY scheduled_for_utc DESC, id DESC LIMIT ?`,
		jobID, limit)
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("list occurrences: %w", err))
	}
	defer func() { _ = rows.Close() }()
	items := []ScheduledOccurrence{}
	for rows.Next() {
		item, err := scanScheduledOccurrence(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, classifyBusy(fmt.Errorf("list occurrences: %w", err))
	}
	return items, nil
}

func scanOneScheduledJob(row rowScanner) (ScheduledJob, bool, error) {
	var job ScheduledJob
	var createdAtRaw, updatedAtRaw string
	err := row.Scan(
		&job.ID, &job.Name, &job.CronExpression, &job.Timezone, &job.Prompt,
		&job.OriginChannel, &job.OriginDestination, &job.OverlapPolicy, &job.CatchUpGraceSeconds,
		&job.State, &job.Version, &createdAtRaw, &updatedAtRaw,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ScheduledJob{}, false, nil
	}
	if err != nil {
		return ScheduledJob{}, false, classifyBusy(fmt.Errorf("load schedule job: %w", err))
	}
	createdAt, err := parseTime(createdAtRaw)
	if err != nil {
		return ScheduledJob{}, false, Errorf(ErrorCodeScheduleInvalid, "job created_at is not a valid timestamp")
	}
	updatedAt, err := parseTime(updatedAtRaw)
	if err != nil {
		return ScheduledJob{}, false, Errorf(ErrorCodeScheduleInvalid, "job updated_at is not a valid timestamp")
	}
	if !validOverlapPolicy(job.OverlapPolicy) || !validJobState(job.State) {
		return ScheduledJob{}, false, Errorf(ErrorCodeScheduleInvalid, "job row carries invalid policy or state")
	}
	job.CreatedAt = createdAt
	job.UpdatedAt = updatedAt
	return job, true, nil
}

func (s *sqliteScheduleStore) requireOccurrence(ctx context.Context, id string) error {
	var present int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM scheduled_occurrence WHERE id = ?`, id).Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		return &Error{Code: ErrorCodeScheduleNotFound, Detail: "occurrence not found"}
	}
	if err != nil {
		return classifyBusy(fmt.Errorf("load occurrence: %w", err))
	}
	return nil
}

func scanScheduledOccurrence(rows *sql.Rows) (ScheduledOccurrence, error) {
	var item ScheduledOccurrence
	var scheduledForRaw, createdAtRaw, updatedAtRaw string
	var turnID, resultEventID, safeErrorCode sql.NullString
	if err := rows.Scan(
		&item.ID, &item.JobID, &item.JobVersion, &scheduledForRaw, &item.State,
		&turnID, &resultEventID, &safeErrorCode, &createdAtRaw, &updatedAtRaw,
	); err != nil {
		return ScheduledOccurrence{}, classifyBusy(fmt.Errorf("scan occurrence: %w", err))
	}
	if !validOccurrenceState(item.State) {
		return ScheduledOccurrence{}, Errorf(ErrorCodeScheduleInvalid, "occurrence row carries invalid state")
	}
	scheduledFor, err := parseTime(scheduledForRaw)
	if err != nil {
		return ScheduledOccurrence{}, Errorf(ErrorCodeScheduleInvalid, "occurrence fire time is not valid")
	}
	createdAt, err := parseTime(createdAtRaw)
	if err != nil {
		return ScheduledOccurrence{}, Errorf(ErrorCodeScheduleInvalid, "occurrence created_at is not valid")
	}
	updatedAt, err := parseTime(updatedAtRaw)
	if err != nil {
		return ScheduledOccurrence{}, Errorf(ErrorCodeScheduleInvalid, "occurrence updated_at is not valid")
	}
	item.ScheduledForUTC = scheduledFor
	item.CreatedAt = createdAt
	item.UpdatedAt = updatedAt
	item.TurnID = turnID.String
	item.ResultEventID = resultEventID.String
	item.SafeErrorCode = safeErrorCode.String
	return item, nil
}

func (s *sqliteScheduleStore) ActiveOccurrence(ctx context.Context, jobID string) (ScheduledOccurrence, bool, error) {
	if s.db == nil {
		return ScheduledOccurrence{}, false, errNilArgument("db")
	}
	if jobID == "" {
		return ScheduledOccurrence{}, false, Errorf(ErrorCodeInvalidArgument, "job id must not be empty")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, job_id, job_version, scheduled_for_utc, state, turn_id,
		result_event_id, safe_error_code, created_at, updated_at
		FROM scheduled_occurrence WHERE job_id = ? AND state = ? ORDER BY scheduled_for_utc DESC, id DESC LIMIT 1`,
		jobID, OccurrenceFired)
	if err != nil {
		return ScheduledOccurrence{}, false, classifyBusy(fmt.Errorf("load active occurrence: %w", err))
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		_ = rows.Close()
		return ScheduledOccurrence{}, false, nil
	}
	item, err := scanScheduledOccurrence(rows)
	if err != nil {
		_ = rows.Close()
		return ScheduledOccurrence{}, false, err
	}
	return item, true, nil
}

func (s *sqliteScheduleStore) AttachTurn(ctx context.Context, id, turnID string, now time.Time) error {
	if s.db == nil {
		return errNilArgument("db")
	}
	if id == "" || turnID == "" {
		return Errorf(ErrorCodeInvalidArgument, "occurrence and turn ids must not be empty")
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE scheduled_occurrence SET turn_id = ?, updated_at = ? WHERE id = ? AND state = ?`,
		turnID, formatTime(now.UTC()), id, OccurrenceFired)
	if err != nil {
		return classifyBusy(fmt.Errorf("attach turn: %w", err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return classifyBusy(fmt.Errorf("attach turn: %w", err))
	}
	if affected == 0 {
		if err := s.requireOccurrence(ctx, id); err != nil {
			return err
		}
		return &Error{Code: ErrorCodeScheduleConflict, Detail: "occurrence is no longer fired"}
	}
	return nil
}

func (s *sqliteScheduleStore) OccurrenceByFire(ctx context.Context, jobID string, scheduledFor time.Time) (ScheduledOccurrence, bool, error) {
	if s.db == nil {
		return ScheduledOccurrence{}, false, errNilArgument("db")
	}
	if jobID == "" {
		return ScheduledOccurrence{}, false, Errorf(ErrorCodeInvalidArgument, "job id must not be empty")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, job_id, job_version, scheduled_for_utc, state, turn_id,
		result_event_id, safe_error_code, created_at, updated_at
		FROM scheduled_occurrence WHERE job_id = ? AND scheduled_for_utc = ?`,
		jobID, formatTime(scheduledFor.UTC()))
	if err != nil {
		return ScheduledOccurrence{}, false, classifyBusy(fmt.Errorf("load occurrence by fire: %w", err))
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		_ = rows.Close()
		return ScheduledOccurrence{}, false, nil
	}
	item, err := scanScheduledOccurrence(rows)
	if err != nil {
		_ = rows.Close()
		return ScheduledOccurrence{}, false, err
	}
	return item, true, nil
}

func (s *sqliteScheduleStore) ActiveJobs(ctx context.Context, limit int) ([]ScheduledJob, error) {
	if s.db == nil {
		return nil, errNilArgument("db")
	}
	if limit <= 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "list limit must be positive")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, cron_expression, timezone, prompt, origin_channel, origin_destination,
		overlap_policy, catch_up_grace_seconds, state, version, created_at, updated_at
		FROM scheduled_job WHERE state = ? ORDER BY id LIMIT ?`,
		ScheduleJobActive, limit)
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("list active jobs: %w", err))
	}
	defer func() { _ = rows.Close() }()
	jobs := []ScheduledJob{}
	for rows.Next() {
		job, found, err := scanOneScheduledJob(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		if !found {
			break
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, classifyBusy(fmt.Errorf("list active jobs: %w", err))
	}
	return jobs, nil
}

func (s *sqliteScheduleStore) PendingFire(ctx context.Context, jobID string) (ScheduledOccurrence, bool, error) {
	if s.db == nil {
		return ScheduledOccurrence{}, false, errNilArgument("db")
	}
	if jobID == "" {
		return ScheduledOccurrence{}, false, Errorf(ErrorCodeInvalidArgument, "job id must not be empty")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, job_id, job_version, scheduled_for_utc, state, turn_id,
		result_event_id, safe_error_code, created_at, updated_at
		FROM scheduled_occurrence
		WHERE job_id = ? AND state = ? AND (turn_id IS NULL OR turn_id = '')
		ORDER BY scheduled_for_utc, id LIMIT 1`,
		jobID, OccurrenceFired)
	if err != nil {
		return ScheduledOccurrence{}, false, classifyBusy(fmt.Errorf("load pending fire: %w", err))
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		_ = rows.Close()
		return ScheduledOccurrence{}, false, nil
	}
	item, err := scanScheduledOccurrence(rows)
	if err != nil {
		_ = rows.Close()
		return ScheduledOccurrence{}, false, err
	}
	return item, true, nil
}

func (s *sqliteScheduleStore) PruneOccurrences(ctx context.Context, jobID string, before time.Time, limit int) (int, error) {
	if s.db == nil {
		return 0, errNilArgument("db")
	}
	if jobID == "" {
		return 0, Errorf(ErrorCodeInvalidArgument, "job id must not be empty")
	}
	if limit <= 0 {
		return 0, Errorf(ErrorCodeInvalidArgument, "prune limit must be positive")
	}
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM scheduled_occurrence WHERE id IN (
			SELECT id FROM scheduled_occurrence
			WHERE job_id = ? AND state IN ('completed','failed','expired','skipped_overlap','cancelled')
			AND updated_at < ? ORDER BY updated_at, id LIMIT ?
		)`,
		jobID, formatTime(before.UTC()), limit)
	if err != nil {
		return 0, classifyBusy(fmt.Errorf("prune occurrences: %w", err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, classifyBusy(fmt.Errorf("prune occurrences: %w", err))
	}
	return int(affected), nil
}

func (s *sqliteScheduleStore) ListJobs(ctx context.Context, limit int) ([]ScheduledJob, error) {
	if s.db == nil {
		return nil, errNilArgument("db")
	}
	if limit <= 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "list limit must be positive")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, cron_expression, timezone, prompt, origin_channel, origin_destination,
		overlap_policy, catch_up_grace_seconds, state, version, created_at, updated_at
		FROM scheduled_job ORDER BY created_at DESC, id DESC LIMIT ?`,
		limit)
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("list jobs: %w", err))
	}
	defer func() { _ = rows.Close() }()
	jobs := []ScheduledJob{}
	for rows.Next() {
		job, found, err := scanOneScheduledJob(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		if !found {
			break
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, classifyBusy(fmt.Errorf("list jobs: %w", err))
	}
	return jobs, nil
}
