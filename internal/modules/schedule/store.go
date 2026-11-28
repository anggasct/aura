package schedule

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

type SQLiteStore struct {
	db *sql.DB
}

func NewSQLiteStore(db *sql.DB) (*SQLiteStore, error) {
	if db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	return &SQLiteStore{db: db}, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func isConstraintUnique(classified error) bool {
	return fault.HasCode(classified, sqlite.ErrorCodeConstraintUnique) ||
		fault.HasCode(classified, sqlite.ErrorCodeConstraintPrimaryKey)
}

func validJobState(state string) bool {
	switch state {
	case JobActive, JobPaused, JobDeleted:
		return true
	default:
		return false
	}
}

const jobColumns = `id, name, cron_expression, timezone, prompt, origin_channel, origin_destination,
		overlap_policy, catch_up_grace_seconds, state, version, created_at, updated_at,
		COALESCE(workflow_id, ''), COALESCE(workflow_input_json, ''),
		context, tainted, COALESCE(taint_source, '')`

func validOverlapPolicy(policy string) bool {
	return policy == OverlapSkip || policy == OverlapQueueOne
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

func validateStoredJob(job *Job) error {
	if job == nil {
		return fault.New(ErrorCodeInvalidArgument, "job must not be nil")
	}
	if job.ID == "" || job.Name == "" || job.CronExpression == "" || job.Timezone == "" {
		return fault.New(ErrorCodeInvalidArgument, "schedule job identity, name, expression, and timezone must not be empty")
	}
	if (job.Prompt == "") == (job.WorkflowID == "") || job.OriginChannel == "" || job.OriginDestination == "" {
		return fault.New(ErrorCodeInvalidArgument, "schedule needs exactly one of a prompt or a workflow, and an origin")
	}
	if !validOverlapPolicy(job.OverlapPolicy) {
		return fault.New(ErrorCodeInvalidArgument, "schedule overlap policy is not valid")
	}
	if job.CatchUpGraceSeconds < 0 {
		return fault.New(ErrorCodeInvalidArgument, "schedule catch-up grace must not be negative")
	}
	if !validJobState(job.State) {
		return fault.New(ErrorCodeInvalidArgument, "schedule job state is not valid")
	}
	if job.Version <= 0 {
		return fault.New(ErrorCodeInvalidArgument, "schedule job version must be positive")
	}
	if !validContext(job.Context) || (job.Context == ContextWebOnly && job.WorkflowID != "") {
		return fault.New(ErrorCodeInvalidArgument, "schedule job context is not valid")
	}
	if job.Tainted != (job.TaintSource != "") || len(job.TaintSource) > maxTaintSourceSize {
		return fault.New(ErrorCodeInvalidArgument, "schedule job taint and its source must agree")
	}
	return nil
}

func (s *SQLiteStore) InsertJob(ctx context.Context, job *Job) error {
	if s == nil || s.db == nil {
		return fault.New(ErrorCodeInvalidArgument, "schedule store must not be nil")
	}
	if err := validateStoredJob(job); err != nil {
		return err
	}
	_, err := sqlite.Conn(ctx, s.db).ExecContext(ctx,
		`INSERT INTO scheduled_job (
			id, name, cron_expression, timezone, prompt, origin_channel, origin_destination,
			overlap_policy, catch_up_grace_seconds, state, version, created_at, updated_at,
			workflow_id, workflow_input_json, context, tainted, taint_source
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		job.ID, job.Name, job.CronExpression, job.Timezone, job.Prompt,
		job.OriginChannel, job.OriginDestination, job.OverlapPolicy, job.CatchUpGraceSeconds,
		job.State, job.Version, sqlite.FormatTime(job.CreatedAt.UTC()), sqlite.FormatTime(job.UpdatedAt.UTC()),
		nullableText(job.WorkflowID), nullableText(job.WorkflowInput),
		job.Context, job.Tainted, nullableText(job.TaintSource),
	)
	if err != nil {
		classified := sqlite.Classify(err)
		if isConstraintUnique(classified) {
			return fault.New(ErrorCodeOccurrenceConflict, fmt.Sprintf("schedule job %q already exists", job.ID))
		}
		return fault.Wrap(ErrorCodeUnavailable, "insert schedule job", classified)
	}
	return nil
}

func (s *SQLiteStore) Job(ctx context.Context, id string) (Job, bool, error) {
	if s == nil || s.db == nil {
		return Job{}, false, fault.New(ErrorCodeInvalidArgument, "schedule store must not be nil")
	}
	return scanOneScheduledJob(sqlite.Conn(ctx, s.db).QueryRowContext(ctx,
		`SELECT `+jobColumns+`
		FROM scheduled_job WHERE id = ?`, id))
}

func (s *SQLiteStore) EditJob(ctx context.Context, id string, edit *JobEdit, expectedVersion int64, now time.Time) (Job, error) {
	if s == nil || s.db == nil {
		return Job{}, fault.New(ErrorCodeInvalidArgument, "schedule store must not be nil")
	}
	if edit == nil {
		return Job{}, fault.New(ErrorCodeInvalidArgument, "edit must not be nil")
	}
	current, found, err := s.Job(ctx, id)
	if err != nil {
		return Job{}, err
	}
	if !found {
		return Job{}, fault.New(ErrorCodeJobNotFound, "schedule job not found")
	}
	candidate := &Job{
		ID: id, Name: edit.Name, CronExpression: edit.CronExpression, Timezone: edit.Timezone,
		Prompt: edit.Prompt, OriginChannel: edit.OriginChannel, OriginDestination: edit.OriginDestination,
		OverlapPolicy: edit.OverlapPolicy, CatchUpGraceSeconds: edit.CatchUpGraceSeconds,
		State: JobActive, Version: expectedVersion + 1, WorkflowID: current.WorkflowID,
		Context: current.Context, Tainted: current.Tainted, TaintSource: current.TaintSource,
	}
	if err := validateStoredJob(candidate); err != nil {
		return Job{}, err
	}
	result, err := sqlite.Conn(ctx, s.db).ExecContext(ctx,
		`UPDATE scheduled_job SET name = ?, cron_expression = ?, timezone = ?, prompt = ?,
		origin_channel = ?, origin_destination = ?, overlap_policy = ?, catch_up_grace_seconds = ?,
		version = ?, updated_at = ? WHERE id = ? AND version = ? AND state != ?`,
		edit.Name, edit.CronExpression, edit.Timezone, edit.Prompt,
		edit.OriginChannel, edit.OriginDestination, edit.OverlapPolicy, edit.CatchUpGraceSeconds,
		expectedVersion+1, sqlite.FormatTime(now.UTC()), id, expectedVersion, JobDeleted)
	if err != nil {
		return Job{}, fault.Wrap(ErrorCodeUnavailable, "edit schedule job", sqlite.Classify(err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return Job{}, fault.Wrap(ErrorCodeUnavailable, "edit schedule job", sqlite.Classify(err))
	}
	if affected == 0 {
		_, stillFound, findErr := s.Job(ctx, id)
		if findErr != nil {
			return Job{}, findErr
		}
		if !stillFound {
			return Job{}, fault.New(ErrorCodeJobNotFound, "schedule job not found")
		}
		return Job{}, fault.New(ErrorCodeOccurrenceConflict, "schedule job changed underneath the edit")
	}
	updated, updatedFound, err := s.Job(ctx, id)
	if err != nil {
		return Job{}, err
	}
	if !updatedFound {
		return Job{}, fault.New(ErrorCodeJobNotFound, "schedule job not found")
	}
	return updated, nil
}

func (s *SQLiteStore) ConfirmJob(ctx context.Context, req *JobConfirmRequest) (Job, error) {
	if s == nil || s.db == nil {
		return Job{}, fault.New(ErrorCodeInvalidArgument, "schedule store must not be nil")
	}
	if req == nil || req.ExpectedVersion <= 0 {
		return Job{}, fault.New(ErrorCodeInvalidArgument, "job confirmation needs the version it was previewed at")
	}
	result, err := sqlite.Conn(ctx, s.db).ExecContext(ctx,
		`UPDATE scheduled_job SET tainted = 0, taint_source = NULL, context = COALESCE(NULLIF(?, ''), context),
		version = version + 1, updated_at = ? WHERE id = ? AND version = ? AND state != ?`,
		req.Context, sqlite.FormatTime(req.At.UTC()), req.ID, req.ExpectedVersion, JobDeleted)
	if err != nil {
		return Job{}, fault.Wrap(ErrorCodeUnavailable, "confirm schedule job", sqlite.Classify(err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return Job{}, fault.Wrap(ErrorCodeUnavailable, "confirm schedule job", sqlite.Classify(err))
	}
	if affected == 0 {
		return Job{}, s.confirmRefusal(ctx, req.ID)
	}
	confirmed, found, err := s.Job(ctx, req.ID)
	if err != nil {
		return Job{}, err
	}
	if !found {
		return Job{}, fault.New(ErrorCodeJobNotFound, "schedule job not found")
	}
	return confirmed, nil
}

func (s *SQLiteStore) confirmRefusal(ctx context.Context, id string) error {
	current, found, err := s.Job(ctx, id)
	switch {
	case err != nil:
		return err
	case !found:
		return fault.New(ErrorCodeJobNotFound, "schedule job not found")
	case current.State == JobDeleted:
		return fault.New(ErrorCodeJobStateInvalid, "deleted jobs are not confirmed")
	}
	return fault.New(ErrorCodeOccurrenceConflict, "schedule job changed after the preview")
}

func (s *SQLiteStore) SetJobState(ctx context.Context, id, state string, now time.Time) error {
	if s == nil || s.db == nil {
		return fault.New(ErrorCodeInvalidArgument, "schedule store must not be nil")
	}
	if !validJobState(state) {
		return fault.New(ErrorCodeInvalidArgument, "schedule job state is not valid")
	}
	result, err := sqlite.Conn(ctx, s.db).ExecContext(ctx,
		`UPDATE scheduled_job SET state = ?, updated_at = ? WHERE id = ? AND state != ?`,
		state, sqlite.FormatTime(now.UTC()), id, JobDeleted)
	if err != nil {
		return fault.Wrap(ErrorCodeUnavailable, "set schedule job state", sqlite.Classify(err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fault.Wrap(ErrorCodeUnavailable, "set schedule job state", sqlite.Classify(err))
	}
	if affected == 0 {
		_, found, findErr := s.Job(ctx, id)
		if findErr != nil {
			return findErr
		}
		if !found {
			return fault.New(ErrorCodeJobNotFound, "schedule job not found")
		}
		return fault.New(ErrorCodeOccurrenceConflict, "schedule job state transition is not allowed")
	}
	return nil
}

func (s *SQLiteStore) RecordFire(ctx context.Context, occurrence *Occurrence) error {
	if s == nil || s.db == nil {
		return fault.New(ErrorCodeInvalidArgument, "schedule store must not be nil")
	}
	if occurrence == nil {
		return fault.New(ErrorCodeInvalidArgument, "occurrence must not be nil")
	}
	if occurrence.ID == "" || occurrence.JobID == "" {
		return fault.New(ErrorCodeInvalidArgument, "occurrence identity must not be empty")
	}
	if !validOccurrenceState(occurrence.State) {
		return fault.New(ErrorCodeInvalidArgument, "occurrence state is not valid")
	}
	_, err := sqlite.Conn(ctx, s.db).ExecContext(ctx,
		`INSERT INTO scheduled_occurrence (
			id, job_id, job_version, scheduled_for_utc, state, turn_id,
			result_event_id, safe_error_code, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		occurrence.ID, occurrence.JobID, occurrence.JobVersion,
		sqlite.FormatTime(occurrence.ScheduledForUTC.UTC()), occurrence.State,
		nullableText(occurrence.TurnID), nullableText(occurrence.ResultEventID), nullableText(occurrence.SafeErrorCode),
		sqlite.FormatTime(occurrence.CreatedAt.UTC()), sqlite.FormatTime(occurrence.UpdatedAt.UTC()),
	)
	if err != nil {
		classified := sqlite.Classify(err)
		if isConstraintUnique(classified) {
			return fault.New(ErrorCodeOccurrenceConflict, "occurrence already recorded for this fire")
		}
		if fault.HasCode(classified, sqlite.ErrorCodeConstraintForeignKey) {
			return fault.New(ErrorCodeJobNotFound, fmt.Sprintf("schedule job %q does not exist", occurrence.JobID))
		}
		return fault.Wrap(ErrorCodeUnavailable, "record occurrence fire", classified)
	}
	return nil
}

func (s *SQLiteStore) SettleOccurrence(ctx context.Context, id, state, turnID, resultEventID, safeErrorCode string, now time.Time) error {
	if s == nil || s.db == nil {
		return fault.New(ErrorCodeInvalidArgument, "schedule store must not be nil")
	}
	if id == "" {
		return fault.New(ErrorCodeInvalidArgument, "occurrence id must not be empty")
	}
	if state != OccurrenceCompleted && state != OccurrenceFailed && state != OccurrenceExpired &&
		state != OccurrenceSkippedOverlap && state != OccurrenceCancelled {
		return fault.New(ErrorCodeInvalidArgument, "settle state is not terminal")
	}
	result, err := sqlite.Conn(ctx, s.db).ExecContext(ctx,
		`UPDATE scheduled_occurrence SET state = ?, turn_id = COALESCE(NULLIF(turn_id, ''), ?),
		result_event_id = ?, safe_error_code = ?, updated_at = ?
		WHERE id = ? AND state = ?`,
		state, nullableText(turnID), nullableText(resultEventID), nullableText(safeErrorCode),
		sqlite.FormatTime(now.UTC()), id, OccurrenceFired)
	if err != nil {
		return fault.Wrap(ErrorCodeUnavailable, "settle occurrence", sqlite.Classify(err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fault.Wrap(ErrorCodeUnavailable, "settle occurrence", sqlite.Classify(err))
	}
	if affected == 0 {
		if err := s.requireOccurrence(ctx, id); err != nil {
			return err
		}
		return fault.New(ErrorCodeOccurrenceConflict, "occurrence is already terminal")
	}
	return nil
}

func (s *SQLiteStore) Occurrences(ctx context.Context, jobID string, limit int) ([]Occurrence, error) {
	if s == nil || s.db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "schedule store must not be nil")
	}
	if jobID == "" {
		return nil, fault.New(ErrorCodeInvalidArgument, "job id must not be empty")
	}
	if limit <= 0 {
		return nil, fault.New(ErrorCodeInvalidArgument, "list limit must be positive")
	}
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx,
		`SELECT id, job_id, job_version, scheduled_for_utc, state, turn_id,
		result_event_id, safe_error_code, created_at, updated_at
		FROM scheduled_occurrence WHERE job_id = ? ORDER BY scheduled_for_utc DESC, id DESC LIMIT ?`,
		jobID, limit)
	if err != nil {
		return nil, fault.Wrap(ErrorCodeUnavailable, "list occurrences", sqlite.Classify(err))
	}
	defer func() { _ = rows.Close() }()
	items := []Occurrence{}
	for rows.Next() {
		item, err := scanScheduledOccurrence(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fault.Wrap(ErrorCodeUnavailable, "list occurrences", sqlite.Classify(err))
	}
	return items, nil
}

func scanOneScheduledJob(row rowScanner) (Job, bool, error) {
	var job Job
	var createdAtRaw, updatedAtRaw string
	err := row.Scan(
		&job.ID, &job.Name, &job.CronExpression, &job.Timezone, &job.Prompt,
		&job.OriginChannel, &job.OriginDestination, &job.OverlapPolicy, &job.CatchUpGraceSeconds,
		&job.State, &job.Version, &createdAtRaw, &updatedAtRaw, &job.WorkflowID, &job.WorkflowInput,
		&job.Context, &job.Tainted, &job.TaintSource,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, fault.Wrap(ErrorCodeUnavailable, "load schedule job", sqlite.Classify(err))
	}
	createdAt, err := sqlite.ParseTime(createdAtRaw)
	if err != nil {
		return Job{}, false, fault.New(ErrorCodeUnavailable, "job created_at is not a valid timestamp")
	}
	updatedAt, err := sqlite.ParseTime(updatedAtRaw)
	if err != nil {
		return Job{}, false, fault.New(ErrorCodeUnavailable, "job updated_at is not a valid timestamp")
	}
	if !validOverlapPolicy(job.OverlapPolicy) || !validJobState(job.State) || !validContext(job.Context) || job.Tainted != (job.TaintSource != "") {
		return Job{}, false, fault.New(ErrorCodeUnavailable, "job row carries invalid policy, state, context or taint")
	}
	job.CreatedAt = createdAt
	job.UpdatedAt = updatedAt
	return job, true, nil
}

func (s *SQLiteStore) requireOccurrence(ctx context.Context, id string) error {
	var present int
	err := sqlite.Conn(ctx, s.db).QueryRowContext(ctx, `SELECT 1 FROM scheduled_occurrence WHERE id = ?`, id).Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		return fault.New(ErrorCodeJobNotFound, "occurrence not found")
	}
	if err != nil {
		return fault.Wrap(ErrorCodeUnavailable, "load occurrence", sqlite.Classify(err))
	}
	return nil
}

func scanScheduledOccurrence(rows *sql.Rows) (Occurrence, error) {
	var item Occurrence
	var scheduledForRaw, createdAtRaw, updatedAtRaw string
	var turnID, resultEventID, safeErrorCode sql.NullString
	if err := rows.Scan(
		&item.ID, &item.JobID, &item.JobVersion, &scheduledForRaw, &item.State,
		&turnID, &resultEventID, &safeErrorCode, &createdAtRaw, &updatedAtRaw,
	); err != nil {
		return Occurrence{}, fault.Wrap(ErrorCodeUnavailable, "scan occurrence", sqlite.Classify(err))
	}
	if !validOccurrenceState(item.State) {
		return Occurrence{}, fault.New(ErrorCodeUnavailable, "occurrence row carries invalid state")
	}
	scheduledFor, err := sqlite.ParseTime(scheduledForRaw)
	if err != nil {
		return Occurrence{}, fault.New(ErrorCodeUnavailable, "occurrence fire time is not valid")
	}
	createdAt, err := sqlite.ParseTime(createdAtRaw)
	if err != nil {
		return Occurrence{}, fault.New(ErrorCodeUnavailable, "occurrence created_at is not valid")
	}
	updatedAt, err := sqlite.ParseTime(updatedAtRaw)
	if err != nil {
		return Occurrence{}, fault.New(ErrorCodeUnavailable, "occurrence updated_at is not valid")
	}
	item.ScheduledForUTC = scheduledFor
	item.CreatedAt = createdAt
	item.UpdatedAt = updatedAt
	item.TurnID = turnID.String
	item.ResultEventID = resultEventID.String
	item.SafeErrorCode = safeErrorCode.String
	return item, nil
}

func (s *SQLiteStore) ActiveOccurrence(ctx context.Context, jobID string) (Occurrence, bool, error) {
	if s == nil || s.db == nil {
		return Occurrence{}, false, fault.New(ErrorCodeInvalidArgument, "schedule store must not be nil")
	}
	if jobID == "" {
		return Occurrence{}, false, fault.New(ErrorCodeInvalidArgument, "job id must not be empty")
	}
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx,
		`SELECT id, job_id, job_version, scheduled_for_utc, state, turn_id,
		result_event_id, safe_error_code, created_at, updated_at
		FROM scheduled_occurrence WHERE job_id = ? AND state = ? ORDER BY scheduled_for_utc DESC, id DESC LIMIT 1`,
		jobID, OccurrenceFired)
	if err != nil {
		return Occurrence{}, false, fault.Wrap(ErrorCodeUnavailable, "load active occurrence", sqlite.Classify(err))
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Occurrence{}, false, fault.Wrap(ErrorCodeUnavailable, "load active occurrence", sqlite.Classify(err))
		}
		return Occurrence{}, false, nil
	}
	item, err := scanScheduledOccurrence(rows)
	if err != nil {
		return Occurrence{}, false, err
	}
	if err := rows.Err(); err != nil {
		return Occurrence{}, false, fault.Wrap(ErrorCodeUnavailable, "load active occurrence", sqlite.Classify(err))
	}
	return item, true, nil
}

func (s *SQLiteStore) AttachTurn(ctx context.Context, id, turnID string, now time.Time) error {
	if s == nil || s.db == nil {
		return fault.New(ErrorCodeInvalidArgument, "schedule store must not be nil")
	}
	if id == "" || turnID == "" {
		return fault.New(ErrorCodeInvalidArgument, "occurrence and turn ids must not be empty")
	}
	result, err := sqlite.Conn(ctx, s.db).ExecContext(ctx,
		`UPDATE scheduled_occurrence SET turn_id = ?, updated_at = ? WHERE id = ? AND state = ?`,
		turnID, sqlite.FormatTime(now.UTC()), id, OccurrenceFired)
	if err != nil {
		return fault.Wrap(ErrorCodeUnavailable, "attach turn", sqlite.Classify(err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fault.Wrap(ErrorCodeUnavailable, "attach turn", sqlite.Classify(err))
	}
	if affected == 0 {
		if err := s.requireOccurrence(ctx, id); err != nil {
			return err
		}
		return fault.New(ErrorCodeOccurrenceConflict, "occurrence is no longer fired")
	}
	return nil
}

func (s *SQLiteStore) OccurrenceByFire(ctx context.Context, jobID string, scheduledFor time.Time) (Occurrence, bool, error) {
	if s == nil || s.db == nil {
		return Occurrence{}, false, fault.New(ErrorCodeInvalidArgument, "schedule store must not be nil")
	}
	if jobID == "" {
		return Occurrence{}, false, fault.New(ErrorCodeInvalidArgument, "job id must not be empty")
	}
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx,
		`SELECT id, job_id, job_version, scheduled_for_utc, state, turn_id,
		result_event_id, safe_error_code, created_at, updated_at
		FROM scheduled_occurrence WHERE job_id = ? AND scheduled_for_utc = ?`,
		jobID, sqlite.FormatTime(scheduledFor.UTC()))
	if err != nil {
		return Occurrence{}, false, fault.Wrap(ErrorCodeUnavailable, "load occurrence by fire", sqlite.Classify(err))
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Occurrence{}, false, fault.Wrap(ErrorCodeUnavailable, "load occurrence by fire", sqlite.Classify(err))
		}
		return Occurrence{}, false, nil
	}
	item, err := scanScheduledOccurrence(rows)
	if err != nil {
		return Occurrence{}, false, err
	}
	if err := rows.Err(); err != nil {
		return Occurrence{}, false, fault.Wrap(ErrorCodeUnavailable, "load occurrence by fire", sqlite.Classify(err))
	}
	return item, true, nil
}

func (s *SQLiteStore) ActiveJobs(ctx context.Context, limit int) ([]Job, error) {
	if s == nil || s.db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "schedule store must not be nil")
	}
	if limit <= 0 {
		return nil, fault.New(ErrorCodeInvalidArgument, "list limit must be positive")
	}
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx,
		`SELECT `+jobColumns+`
		FROM scheduled_job WHERE state = ? ORDER BY id LIMIT ?`,
		JobActive, limit)
	if err != nil {
		return nil, fault.Wrap(ErrorCodeUnavailable, "list active jobs", sqlite.Classify(err))
	}
	defer func() { _ = rows.Close() }()
	jobs := []Job{}
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
		return nil, fault.Wrap(ErrorCodeUnavailable, "list active jobs", sqlite.Classify(err))
	}
	return jobs, nil
}

func (s *SQLiteStore) FiredOccurrences(ctx context.Context, limit int) ([]Occurrence, error) {
	if s == nil || s.db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "schedule store must not be nil")
	}
	if limit <= 0 {
		return nil, fault.New(ErrorCodeInvalidArgument, "list limit must be positive")
	}
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx,
		`SELECT id, job_id, job_version, scheduled_for_utc, state, turn_id,
		result_event_id, safe_error_code, created_at, updated_at
		FROM scheduled_occurrence WHERE state = ? ORDER BY scheduled_for_utc, id LIMIT ?`,
		OccurrenceFired, limit)
	if err != nil {
		return nil, fault.Wrap(ErrorCodeUnavailable, "list fired occurrences", sqlite.Classify(err))
	}
	defer func() { _ = rows.Close() }()
	items := []Occurrence{}
	for rows.Next() {
		item, err := scanScheduledOccurrence(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fault.Wrap(ErrorCodeUnavailable, "list fired occurrences", sqlite.Classify(err))
	}
	return items, nil
}

func (s *SQLiteStore) PruneOccurrences(ctx context.Context, jobID string, before time.Time, limit int) (int, error) {
	if s == nil || s.db == nil {
		return 0, fault.New(ErrorCodeInvalidArgument, "schedule store must not be nil")
	}
	if jobID == "" {
		return 0, fault.New(ErrorCodeInvalidArgument, "job id must not be empty")
	}
	if limit <= 0 {
		return 0, fault.New(ErrorCodeInvalidArgument, "prune limit must be positive")
	}
	result, err := sqlite.Conn(ctx, s.db).ExecContext(ctx,
		`DELETE FROM scheduled_occurrence WHERE id IN (
			SELECT id FROM scheduled_occurrence
			WHERE job_id = ? AND state IN ('completed','failed','expired','skipped_overlap','cancelled')
			AND updated_at < ? ORDER BY updated_at, id LIMIT ?
		)`,
		jobID, sqlite.FormatTime(before.UTC()), limit)
	if err != nil {
		return 0, fault.Wrap(ErrorCodeUnavailable, "prune occurrences", sqlite.Classify(err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fault.Wrap(ErrorCodeUnavailable, "prune occurrences", sqlite.Classify(err))
	}
	return int(affected), nil
}

func (s *SQLiteStore) ListJobs(ctx context.Context, limit int) ([]Job, error) {
	if s == nil || s.db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "schedule store must not be nil")
	}
	if limit <= 0 {
		return nil, fault.New(ErrorCodeInvalidArgument, "list limit must be positive")
	}
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx,
		`SELECT `+jobColumns+`
		FROM scheduled_job ORDER BY created_at DESC, id DESC LIMIT ?`,
		limit)
	if err != nil {
		return nil, fault.Wrap(ErrorCodeUnavailable, "list jobs", sqlite.Classify(err))
	}
	defer func() { _ = rows.Close() }()
	jobs := []Job{}
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
		return nil, fault.Wrap(ErrorCodeUnavailable, "list jobs", sqlite.Classify(err))
	}
	return jobs, nil
}

func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}
