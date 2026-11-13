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

	"modernc.org/sqlite"

	"github.com/anggasct/aura/internal/store"
)

type Store struct {
	db       *sql.DB
	newRunID func() string
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db, newRunID: func() string { return newID("run") }}
}

func RunSessionID(runID string) string {
	return "wf-" + runID
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

const (
	sqliteConstraintUnique     = 2067 // SQLITE_CONSTRAINT_UNIQUE
	sqliteConstraintPrimaryKey = 1555 // SQLITE_CONSTRAINT_PRIMARYKEY
	sqliteConstraint           = 19   // SQLITE_CONSTRAINT
)

func isConstraintUnique(err error) bool {
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		code := sqliteErr.Code()
		return code == sqliteConstraintUnique || code == sqliteConstraintPrimaryKey || code == sqliteConstraint
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func (s *Store) beginImmediate(ctx context.Context) (*sql.Conn, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("open connection: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("begin immediate transaction: %w", err)
	}
	return conn, nil
}

func (s *Store) SaveDefinition(ctx context.Context, spec *Spec) error {
	return s.saveDefinition(ctx, spec, nil)
}

func (s *Store) saveDefinition(ctx context.Context, spec *Spec, approval *Approval) error {
	encoded, err := specJSON(spec)
	if err != nil {
		return err
	}
	digest := sha256Hex(encoded)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	conn, err := s.beginImmediate(ctx)
	if err != nil {
		return fmt.Errorf("begin definition save: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		_ = conn.Close()
	}()
	var stored string
	err = conn.QueryRowContext(ctx,
		`SELECT spec_sha256 FROM workflow_definition WHERE id = ? AND version = ?`,
		spec.ID, spec.Version,
	).Scan(&stored)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO workflow_definition (id, version, goal, source, spec_json, spec_sha256, created_at, name) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			spec.ID, spec.Version, spec.Goal, string(spec.Source), string(encoded), digest, now, nullable(spec.Name),
		); err != nil {
			if isConstraintUnique(err) {
				return codedError(ErrorCodeSpecInvalid, fmt.Sprintf("definition %s version %d already exists with different content", spec.ID, spec.Version))
			}
			return fmt.Errorf("insert definition: %w", err)
		}
	case err != nil:
		return fmt.Errorf("read definition: %w", err)
	case stored != digest:
		return codedError(ErrorCodeSpecInvalid, fmt.Sprintf("definition %s version %d already exists with different content", spec.ID, spec.Version))
	}
	if approval != nil {
		record := *approval
		record.DefinitionID, record.DefinitionVersion, record.Digest = spec.ID, spec.Version, digest
		record.ApprovedAt = time.Now().UTC()
		if err := insertApproval(ctx, conn, &record); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit definition save: %w", err)
	}
	return nil
}

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

type runCreate struct {
	Spec            *Spec
	Input           *RunInput
	Origin          *RunOrigin
	DurableKey      string
	ParentRunID     string
	ClaimProposalID string
}

func (s *Store) createRun(ctx context.Context, create *runCreate) (*RunSummary, error) {
	spec, origin := create.Spec, create.Origin
	if !origin.Complete() {
		return nil, codedError(ErrorCodeOriginIncomplete, "a run needs a principal, a known principal class and an agent")
	}
	encodedInput, err := json.Marshal(create.Input)
	if err != nil {
		return nil, codedError(ErrorCodeSpecInvalid, "encode run input: "+err.Error())
	}
	now := time.Now().UTC()
	summary := &RunSummary{
		Origin:            *origin,
		ID:                s.newRunID(),
		DefinitionID:      spec.ID,
		DefinitionVersion: spec.Version,
		DurableKey:        create.DurableKey,
		ParentRunID:       create.ParentRunID,
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
	if err := store.CreateSessionTx(ctx, tx, &store.Session{
		ID: RunSessionID(summary.ID), OwnerID: origin.PrincipalID, AgentID: origin.AgentID,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		if code, ok := store.CodeOf(err); ok && code == store.ErrorCodeSessionIDConflict {
			return nil, codedError(ErrorCodeSessionInvalid, "session "+RunSessionID(summary.ID)+" already exists")
		}
		return nil, fmt.Errorf("create run session: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO workflow_run (id, definition_id, definition_version, durable_key, goal, input_json, status, created_at, updated_at, principal_id, principal_class, agent_id, origin_session_id, reply_account, reply_channel_id, tainted, parent_run_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		summary.ID, summary.DefinitionID, summary.DefinitionVersion, summary.DurableKey, summary.Goal,
		string(encodedInput), summary.Status,
		summary.CreatedAt.Format(time.RFC3339Nano), summary.UpdatedAt.Format(time.RFC3339Nano),
		nullable(summary.Origin.PrincipalID), nullable(summary.Origin.PrincipalClass), nullable(summary.Origin.AgentID), nullable(summary.Origin.SessionID),
		nullable(summary.Origin.ReplyAccount), nullable(summary.Origin.ReplyChannelID),
		summary.Origin.Tainted, nullable(summary.ParentRunID),
	); err != nil {
		return nil, fmt.Errorf("insert run: %w", err)
	}
	if create.ClaimProposalID != "" {
		if err := claimProposalRun(ctx, tx, create.ClaimProposalID, summary.ID); err != nil {
			return nil, err
		}
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

func claimProposalRun(ctx context.Context, tx *sql.Tx, proposalID, runID string) error {
	result, err := tx.ExecContext(ctx, `UPDATE workflow_proposal SET run_id = ? WHERE id = ? AND run_id IS NULL`, runID, proposalID)
	if err != nil {
		return fmt.Errorf("claim proposal run: %w", err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed == 1 {
		return err
	}
	var tainted bool
	err = tx.QueryRowContext(ctx, `SELECT tainted FROM workflow_proposal WHERE id = ?`, proposalID).Scan(&tainted)
	switch {
	case err == nil && !tainted:
		return nil
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("read proposal taint: %w", err)
	}
	return codedError(ErrorCodeApprovalSpent, "the approval of proposal "+proposalID+" already ran")
}

func (s *Store) Run(ctx context.Context, runID string) (*RunSummary, error) {
	var summary RunSummary
	var createdAt, updatedAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, definition_id, definition_version, COALESCE(durable_key, ''), goal, status, created_at, updated_at,
			`+runOriginColumns+` FROM workflow_run WHERE id = ?`,
		runID,
	).Scan(&summary.ID, &summary.DefinitionID, &summary.DefinitionVersion, &summary.DurableKey, &summary.Goal, &summary.Status, &createdAt, &updatedAt,
		&summary.Origin.PrincipalID, &summary.Origin.PrincipalClass, &summary.Origin.AgentID, &summary.Origin.SessionID, &summary.Origin.ReplyAccount, &summary.Origin.ReplyChannelID, &summary.Origin.Tainted, &summary.ParentRunID)
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

func (s *Store) Runs(ctx context.Context, status string) ([]*RunSummary, error) {
	query := `SELECT id, definition_id, definition_version, COALESCE(durable_key, ''), goal, status, created_at, updated_at FROM workflow_run`
	args := []any{}
	if status != "" {
		query += ` WHERE status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY id`
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

func (s *Store) SetRunStatus(ctx context.Context, runID, status string) error {
	return s.exec(ctx,
		`UPDATE workflow_run SET status = ?, updated_at = ? WHERE id = ?`,
		status, time.Now().UTC().Format(time.RFC3339Nano), runID,
	)
}

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
	Tokens         int64
}

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

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

const runOriginColumns = `COALESCE(principal_id, ''), COALESCE(principal_class, ''), COALESCE(agent_id, ''), COALESCE(origin_session_id, ''),
	COALESCE(reply_account, ''), COALESCE(reply_channel_id, ''), tainted, COALESCE(parent_run_id, '')`

const MaxCompletionAttempts = 5

func (s *Store) PendingCompletions(ctx context.Context, limit int) ([]*RunSummary, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, definition_id, definition_version, COALESCE(durable_key, ''), goal, status, created_at, updated_at, `+runOriginColumns+`
		FROM workflow_run WHERE origin_session_id IS NOT NULL AND completion_notified_at IS NULL AND parent_run_id IS NULL AND status IN (?, ?, ?)
		ORDER BY updated_at, id LIMIT ?`, RunSucceeded, RunFailed, RunCancelled, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending completions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var runs []*RunSummary
	for rows.Next() {
		var summary RunSummary
		var createdAt, updatedAt string
		if err := rows.Scan(&summary.ID, &summary.DefinitionID, &summary.DefinitionVersion, &summary.DurableKey, &summary.Goal, &summary.Status, &createdAt, &updatedAt,
			&summary.Origin.PrincipalID, &summary.Origin.PrincipalClass, &summary.Origin.AgentID, &summary.Origin.SessionID, &summary.Origin.ReplyAccount, &summary.Origin.ReplyChannelID, &summary.Origin.Tainted, &summary.ParentRunID); err != nil {
			return nil, fmt.Errorf("scan pending completion: %w", err)
		}
		summary.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		summary.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
		runs = append(runs, &summary)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending completions: %w", err)
	}
	return runs, nil
}

func (s *Store) ClaimCompletion(ctx context.Context, runID string, at time.Time) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE workflow_run SET completion_notified_at = ? WHERE id = ? AND completion_notified_at IS NULL`,
		at.UTC().Format(time.RFC3339Nano), runID)
	if err != nil {
		return false, fmt.Errorf("claim completion: %w", err)
	}
	claimed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim completion: %w", err)
	}
	return claimed == 1, nil
}

func (s *Store) ReleaseCompletion(ctx context.Context, runID string) (exhausted bool, err error) {
	var attempts int
	err = s.db.QueryRowContext(ctx, `UPDATE workflow_run SET completion_attempts = completion_attempts + 1,
		completion_notified_at = CASE WHEN completion_attempts + 1 >= ? THEN completion_notified_at ELSE NULL END
		WHERE id = ? RETURNING completion_attempts`, MaxCompletionAttempts, runID).Scan(&attempts)
	if err != nil {
		return false, fmt.Errorf("release completion: %w", err)
	}
	return attempts >= MaxCompletionAttempts, nil
}

func (s *Store) CompletionAttempts(ctx context.Context, runID string) (int, error) {
	var attempts int
	if err := s.db.QueryRowContext(ctx, `SELECT completion_attempts FROM workflow_run WHERE id = ?`, runID).Scan(&attempts); err != nil {
		return 0, fmt.Errorf("read completion attempts: %w", err)
	}
	return attempts, nil
}

func (s *Store) runStatus(ctx context.Context, runID string) (string, error) {
	var status string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM workflow_run WHERE id = ?`, runID).Scan(&status); err != nil {
		return "", fmt.Errorf("read run status: %w", err)
	}
	return status, nil
}

func (s *Store) LatestDefinition(ctx context.Context, id string) (*Spec, error) {
	var encoded string
	err := s.db.QueryRowContext(ctx, `SELECT spec_json FROM workflow_definition WHERE id = ? ORDER BY version DESC LIMIT 1`, id).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, codedError(ErrorCodeDefinitionNotFound, "definition "+id+" is not registered")
	}
	if err != nil {
		return nil, fmt.Errorf("read latest definition: %w", err)
	}
	return decodeSpec(encoded)
}

func (s *Store) Routines(ctx context.Context) ([]*Spec, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT d.spec_json FROM workflow_definition d
		WHERE d.name IS NOT NULL AND d.source = ? AND d.version = (SELECT MAX(version) FROM workflow_definition WHERE id = d.id)
		ORDER BY d.id`, string(SourceGenerated))
	if err != nil {
		return nil, fmt.Errorf("list routines: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var routines []*Spec
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			return nil, fmt.Errorf("scan routine: %w", err)
		}
		spec, err := decodeSpec(encoded)
		if err != nil {
			return nil, err
		}
		routines = append(routines, spec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate routines: %w", err)
	}
	return routines, nil
}

func (s *Store) RunByDurableKey(ctx context.Context, durableKey string) (*RunSummary, bool, error) {
	var runID string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM workflow_run WHERE durable_key = ?`, durableKey).Scan(&runID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read run by durable key: %w", err)
	}
	run, err := s.Run(ctx, runID)
	return run, err == nil, err
}
