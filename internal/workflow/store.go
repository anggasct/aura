package workflow

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Store persists workflow definitions and run/step projections.
type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

func newID(prefix string) string {
	return prefix + "-" + rand.Text()
}

func specJSON(spec *Spec) ([]byte, error) {
	encoded, err := json.Marshal(spec)
	if err != nil {
		return nil, codedError(ErrorCodeSpecInvalid, "encode spec: "+err.Error())
	}
	return encoded, nil
}

// SaveDefinition persists one spec version idempotently by
// (id, version, sha256); identical content is a no-op. A different spec
// already stored under the same (id, version) is rejected.
func (s *Store) SaveDefinition(ctx context.Context, spec *Spec) error {
	encoded, err := specJSON(spec)
	if err != nil {
		return err
	}
	digest := sha256Hex(encoded)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin definition save: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var stored string
	err = tx.QueryRowContext(ctx,
		`SELECT spec_sha256 FROM workflow_definition WHERE id = ? AND version = ?`,
		spec.ID, spec.Version,
	).Scan(&stored)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO workflow_definition (id, version, goal, source, spec_json, spec_sha256, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			spec.ID, spec.Version, spec.Goal, string(spec.Source), string(encoded), digest, now,
		); err != nil {
			return fmt.Errorf("insert definition: %w", err)
		}
	case err != nil:
		return fmt.Errorf("read definition: %w", err)
	case stored != digest:
		return codedError(ErrorCodeSpecInvalid, fmt.Sprintf("definition %s version %d already exists with different content", spec.ID, spec.Version))
	}
	return tx.Commit()
}

// Definition returns one stored spec version.
func (s *Store) Definition(ctx context.Context, id string, version int) (*Spec, error) {
	var encoded string
	err := s.db.QueryRowContext(ctx,
		`SELECT spec_json FROM workflow_definition WHERE id = ? AND version = ?`,
		id, version,
	).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, codedError(ErrorCodeDefinitionNotFound, fmt.Sprintf("definition %s version %d is not registered", id, version))
	}
	if err != nil {
		return nil, fmt.Errorf("read definition: %w", err)
	}
	return decodeSpec(encoded)
}

// Definitions lists stored specs by (id, version).
func (s *Store) Definitions(ctx context.Context) ([]*Spec, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT spec_json FROM workflow_definition ORDER BY id, version`)
	if err != nil {
		return nil, fmt.Errorf("list definitions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var specs []*Spec
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			return nil, fmt.Errorf("scan definition: %w", err)
		}
		spec, err := decodeSpec(encoded)
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate definitions: %w", err)
	}
	return specs, nil
}

func decodeSpec(encoded string) (*Spec, error) {
	var spec Spec
	if err := json.Unmarshal([]byte(encoded), &spec); err != nil {
		return nil, codedError(ErrorCodeSpecInvalid, "decode stored spec: "+err.Error())
	}
	return &spec, nil
}

// CreateRun persists a queued run plus pending rows for every step; the
// returned run id identifies the projection while durable_key links the
// durable execution.
func (s *Store) CreateRun(ctx context.Context, spec *Spec, input *RunInput) (*RunSummary, error) {
	encodedInput, err := json.Marshal(input)
	if err != nil {
		return nil, codedError(ErrorCodeSpecInvalid, "encode run input: "+err.Error())
	}
	now := time.Now().UTC()
	summary := &RunSummary{
		ID:                newID("run"),
		DefinitionID:      spec.ID,
		DefinitionVersion: spec.Version,
		DurableKey:        newID("dur"),
		Goal:              spec.Goal,
		Status:            RunQueued,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin run create: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO workflow_run (id, definition_id, definition_version, durable_key, goal, input_json, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		summary.ID, summary.DefinitionID, summary.DefinitionVersion, summary.DurableKey, summary.Goal,
		string(encodedInput), summary.Status,
		summary.CreatedAt.Format(time.RFC3339Nano), summary.UpdatedAt.Format(time.RFC3339Nano),
	); err != nil {
		return nil, fmt.Errorf("insert run: %w", err)
	}
	for index := range spec.Steps {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO workflow_step_run (run_id, step_id, status, attempt, updated_at) VALUES (?, ?, ?, 0, ?)`,
			summary.ID, spec.Steps[index].ID, StepPending, summary.UpdatedAt.Format(time.RFC3339Nano),
		); err != nil {
			return nil, fmt.Errorf("insert step row: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit run create: %w", err)
	}
	return summary, nil
}

// Run returns one run summary.
func (s *Store) Run(ctx context.Context, runID string) (*RunSummary, error) {
	var summary RunSummary
	var createdAt, updatedAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, definition_id, definition_version, COALESCE(durable_key, ''), goal, status, created_at, updated_at FROM workflow_run WHERE id = ?`,
		runID,
	).Scan(&summary.ID, &summary.DefinitionID, &summary.DefinitionVersion, &summary.DurableKey, &summary.Goal, &summary.Status, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, codedError(ErrorCodeRunNotFound, "run "+runID+" is not registered")
	}
	if err != nil {
		return nil, fmt.Errorf("read run: %w", err)
	}
	summary.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	summary.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
	return &summary, nil
}

// Runs lists runs newest-first, optionally filtered by status.
func (s *Store) Runs(ctx context.Context, status string) ([]*RunSummary, error) {
	query := `SELECT id, definition_id, definition_version, COALESCE(durable_key, ''), goal, status, created_at, updated_at FROM workflow_run`
	args := []any{}
	if status != "" {
		query += ` WHERE status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY created_at DESC, id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var runs []*RunSummary
	for rows.Next() {
		var summary RunSummary
		var createdAt, updatedAt string
		if err := rows.Scan(&summary.ID, &summary.DefinitionID, &summary.DefinitionVersion, &summary.DurableKey, &summary.Goal, &summary.Status, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		summary.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		summary.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
		runs = append(runs, &summary)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate runs: %w", err)
	}
	return runs, nil
}

// Steps lists one run's step rows in compiled order (sorted by step id; the
// interpreter reorders by graph).
func (s *Store) Steps(ctx context.Context, runID string) ([]*StepRun, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT step_id, status, attempt, started_at, ended_at, COALESCE(output_json, ''), COALESCE(output_artifact_digest, ''), COALESCE(error_code, ''), updated_at FROM workflow_step_run WHERE run_id = ? ORDER BY step_id`,
		runID,
	)
	if err != nil {
		return nil, fmt.Errorf("list steps: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var steps []*StepRun
	for rows.Next() {
		step := &StepRun{RunID: runID}
		var startedAt, endedAt, updatedAt sql.NullString
		var output string
		if err := rows.Scan(&step.StepID, &step.Status, &step.Attempt, &startedAt, &endedAt, &output, &step.OutputArtifactDigest, &step.ErrorCode, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan step: %w", err)
		}
		step.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt.String)
		if startedAt.Valid {
			parsed, _ := time.Parse(time.RFC3339Nano, startedAt.String)
			step.StartedAt = &parsed
		}
		if endedAt.Valid {
			parsed, _ := time.Parse(time.RFC3339Nano, endedAt.String)
			step.EndedAt = &parsed
		}
		if output != "" {
			step.Output = []byte(output)
		}
		steps = append(steps, step)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate steps: %w", err)
	}
	return steps, nil
}

// SetRunStatus persists one durable run-state transition.
func (s *Store) SetRunStatus(ctx context.Context, runID, status string) error {
	return s.exec(ctx,
		`UPDATE workflow_run SET status = ?, updated_at = ? WHERE id = ?`,
		status, time.Now().UTC().Format(time.RFC3339Nano), runID,
	)
}

// SetRunStatusTx persists the transition inside the caller's transaction.
func (s *Store) SetRunStatusTx(ctx context.Context, tx *sql.Tx, runID, status string) error {
	result, err := tx.ExecContext(ctx,
		`UPDATE workflow_run SET status = ?, updated_at = ? WHERE id = ?`,
		status, time.Now().UTC().Format(time.RFC3339Nano), runID,
	)
	if err != nil {
		return fmt.Errorf("update run status: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read run update: %w", err)
	}
	if affected == 0 {
		return codedError(ErrorCodeRunNotFound, "run "+runID+" is not registered")
	}
	return nil
}

// BeginStepTransaction exposes a transaction for the interpreter's
// terminal step transition: the step row and, on run-terminal steps, the
// run row commit atomically.
func (s *Store) BeginStepTransaction(ctx context.Context) (*sql.Tx, error) {
	return s.db.BeginTx(ctx, nil)
}

type stepUpdate struct {
	Status         string
	Attempt        int
	StartedAt      *time.Time
	EndedAt        *time.Time
	Output         []byte
	ArtifactDigest string
	ErrorCode      string
	Detail         string
}

// UpdateStep persists one step transition inside tx.
func UpdateStep(ctx context.Context, tx *sql.Tx, runID, stepID string, update *stepUpdate) error {
	format := func(t *time.Time) any {
		if t == nil {
			return nil
		}
		return t.UTC().Format(time.RFC3339Nano)
	}
	var output any
	if len(update.Output) > 0 {
		output = string(update.Output)
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE workflow_step_run SET status = ?, attempt = ?, started_at = ?, ended_at = ?, output_json = ?, output_artifact_digest = ?, error_code = ?, updated_at = ? WHERE run_id = ? AND step_id = ?`,
		update.Status, update.Attempt, format(update.StartedAt), format(update.EndedAt), output, nullIfEmpty(update.ArtifactDigest), nullIfEmpty(update.ErrorCode), time.Now().UTC().Format(time.RFC3339Nano), runID, stepID,
	)
	if err != nil {
		return fmt.Errorf("update step %s: %w", stepID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read step update: %w", err)
	}
	if affected == 0 {
		return codedError(ErrorCodeRunNotFound, fmt.Sprintf("run %s has no step %s", runID, stepID))
	}
	return nil
}

func (s *Store) exec(ctx context.Context, query string, args ...any) error {
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("workflow store exec: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read exec result: %w", err)
	}
	if affected == 0 {
		return codedError(ErrorCodeRunNotFound, "no matching workflow row")
	}
	return nil
}

func nullIfEmpty(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

// RunInputFor reads one run's stored input.
func (s *Store) RunInputFor(ctx context.Context, runID string) (*RunInput, error) {
	var encoded string
	err := s.db.QueryRowContext(ctx, `SELECT input_json FROM workflow_run WHERE id = ?`, runID).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, codedError(ErrorCodeRunNotFound, "run "+runID+" is not registered")
	}
	if err != nil {
		return nil, fmt.Errorf("read run input: %w", err)
	}
	var input RunInput
	if err := json.Unmarshal([]byte(encoded), &input); err != nil {
		return nil, codedError(ErrorCodeSpecInvalid, "decode run input: "+err.Error())
	}
	return &input, nil
}
