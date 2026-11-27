package workflow

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

type SessionCreator interface {
	CreateSession(ctx context.Context, sess *turn.Session) error
}

type Store struct {
	db       *sql.DB
	sessions SessionCreator
	newRunID func() string
}

func NewStore(db *sql.DB, sessions SessionCreator) (*Store, error) {
	if db == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "workflow store requires a database")
	}
	if sessions == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "workflow store requires a session creator")
	}
	return &Store{db: db, sessions: sessions, newRunID: func() string { return newID("run") }}, nil
}

func RunSessionID(runID string) string {
	return "wf-" + runID
}

func StepSessionID(runID, stepID string, attempt int) string {
	return RunSessionID(runID) + "-" + stepID + "-" + strconv.Itoa(attempt)
}

func newID(prefix string) string {
	return prefix + "-" + rand.Text()
}

func withAgentDefaults(spec *Spec) *Spec {
	canonical := *spec
	canonical.Steps = slices.Clone(spec.Steps)
	for index := range canonical.Steps {
		if canonical.Steps[index].Executor.Kind == KindAgent {
			canonical.Steps[index].Executor.applyAgentDefaults()
		}
	}
	return &canonical
}

func specJSON(spec *Spec) ([]byte, error) {
	encoded, err := json.Marshal(withAgentDefaults(spec))
	if err != nil {
		return nil, codedError(ErrorCodeSpecInvalid, "encode spec: "+err.Error())
	}
	return encoded, nil
}

func isConstraintUnique(err error) bool {
	classified := sqlite.Classify(err)
	return fault.HasCode(classified, sqlite.ErrorCodeConstraintUnique) ||
		fault.HasCode(classified, sqlite.ErrorCodeConstraintPrimaryKey)
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
	return sqlite.RunInTx(ctx, s.db, func(ctx context.Context) error {
		q := sqlite.Conn(ctx, s.db)
		var stored string
		err := q.QueryRowContext(ctx,
			`SELECT spec_sha256 FROM workflow_definition WHERE id = ? AND version = ?`,
			spec.ID, spec.Version,
		).Scan(&stored)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := q.ExecContext(ctx,
				`INSERT INTO workflow_definition (id, version, goal, source, spec_json, spec_sha256, created_at, name) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				spec.ID, spec.Version, spec.Goal, string(spec.Source), string(encoded), digest, now, nullable(spec.Name),
			); err != nil {
				if isConstraintUnique(err) {
					return codedError(ErrorCodeSpecInvalid, fmt.Sprintf("definition %s version %d already exists with different content", spec.ID, spec.Version))
				}
				return sqlite.Classify(fmt.Errorf("insert definition: %w", err))
			}
		case err != nil:
			return sqlite.Classify(fmt.Errorf("read definition: %w", err))
		case stored != digest:
			return codedError(ErrorCodeSpecInvalid, fmt.Sprintf("definition %s version %d already exists with different content", spec.ID, spec.Version))
		}
		if approval != nil {
			record := *approval
			record.DefinitionID, record.DefinitionVersion, record.Digest = spec.ID, spec.Version, digest
			record.ApprovedAt = time.Now().UTC()
			if err := insertApproval(ctx, q, &record); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) Definition(ctx context.Context, id string, version int) (*Spec, error) {
	var encoded string
	err := sqlite.Conn(ctx, s.db).QueryRowContext(ctx,
		`SELECT spec_json FROM workflow_definition WHERE id = ? AND version = ?`,
		id, version,
	).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, codedError(ErrorCodeDefinitionNotFound, fmt.Sprintf("definition %s version %d is not registered", id, version))
	}
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("read definition: %w", err))
	}
	return decodeSpec(encoded)
}

func (s *Store) Definitions(ctx context.Context) ([]*Spec, error) {
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx, `SELECT spec_json FROM workflow_definition ORDER BY id, version`)
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list definitions: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var specs []*Spec
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			return nil, sqlite.Classify(fmt.Errorf("scan definition: %w", err))
		}
		spec, err := decodeSpec(encoded)
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("iterate definitions: %w", err))
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
	if err := origin.Reply.Validate(); err != nil {
		return nil, err
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
	if err := sqlite.RunInTx(ctx, s.db, func(ctx context.Context) error {
		q := sqlite.Conn(ctx, s.db)
		if err := s.sessions.CreateSession(ctx, &turn.Session{
			ID: RunSessionID(summary.ID), OwnerID: origin.PrincipalID, AgentID: origin.AgentID,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			if fault.HasCode(err, turnstore.ErrorCodeSessionIDConflict) {
				return codedError(ErrorCodeSessionInvalid, "session "+RunSessionID(summary.ID)+" already exists")
			}
			return sqlite.Classify(fmt.Errorf("create run session: %w", err))
		}
		if _, err := q.ExecContext(ctx,
			`INSERT INTO workflow_run (id, definition_id, definition_version, durable_key, goal, input_json, status, created_at, updated_at, principal_id, principal_class, agent_id, origin_session_id, reply_channel, reply_account, reply_channel_id, reply_thread_id, tainted, parent_run_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			summary.ID, summary.DefinitionID, summary.DefinitionVersion, summary.DurableKey, summary.Goal,
			string(encodedInput), summary.Status,
			summary.CreatedAt.Format(time.RFC3339Nano), summary.UpdatedAt.Format(time.RFC3339Nano),
			nullable(summary.Origin.PrincipalID), nullable(summary.Origin.PrincipalClass), nullable(summary.Origin.AgentID), nullable(summary.Origin.SessionID),
			nullable(summary.Origin.Reply.Channel), nullable(summary.Origin.Reply.Account), nullable(summary.Origin.Reply.ChannelID), nullable(summary.Origin.Reply.ThreadID),
			summary.Origin.Tainted, nullable(summary.ParentRunID),
		); err != nil {
			return sqlite.Classify(fmt.Errorf("insert run: %w", err))
		}
		if create.ClaimProposalID != "" {
			if err := claimProposalRun(ctx, q, create.ClaimProposalID, summary.ID); err != nil {
				return err
			}
		}
		for index := range spec.Steps {
			if _, err := q.ExecContext(ctx,
				`INSERT INTO workflow_step_run (run_id, step_id, status, attempt, updated_at) VALUES (?, ?, ?, 0, ?)`,
				summary.ID, spec.Steps[index].ID, StepPending, summary.UpdatedAt.Format(time.RFC3339Nano),
			); err != nil {
				return sqlite.Classify(fmt.Errorf("insert step row: %w", err))
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return summary, nil
}

type stepSession struct {
	ID        string
	OwnerID   string
	PersonaID string
}

func (s *Store) createStepSession(ctx context.Context, session *stepSession) error {
	now := time.Now().UTC()
	err := s.sessions.CreateSession(ctx, &turn.Session{ID: session.ID, OwnerID: session.OwnerID, AgentID: session.PersonaID, CreatedAt: now, UpdatedAt: now})
	if err != nil && !fault.HasCode(err, turnstore.ErrorCodeSessionIDConflict) {
		return sqlite.Classify(fmt.Errorf("create step session: %w", err))
	}
	return nil
}

func claimProposalRun(ctx context.Context, q sqlite.Querier, proposalID, runID string) error {
	result, err := q.ExecContext(ctx, `UPDATE workflow_proposal SET run_id = ? WHERE id = ? AND run_id IS NULL`, runID, proposalID)
	if err != nil {
		return sqlite.Classify(fmt.Errorf("claim proposal run: %w", err))
	}
	if changed, err := result.RowsAffected(); err != nil || changed == 1 {
		return sqlite.Classify(err)
	}
	var tainted bool
	err = q.QueryRowContext(ctx, `SELECT tainted FROM workflow_proposal WHERE id = ?`, proposalID).Scan(&tainted)
	switch {
	case err == nil && !tainted:
		return nil
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return sqlite.Classify(fmt.Errorf("read proposal taint: %w", err))
	}
	return codedError(ErrorCodeApprovalSpent, "the approval of proposal "+proposalID+" already ran")
}

func (s *Store) Run(ctx context.Context, runID string) (*RunSummary, error) {
	var summary RunSummary
	var createdAt, updatedAt string
	err := sqlite.Conn(ctx, s.db).QueryRowContext(ctx,
		`SELECT id, definition_id, definition_version, COALESCE(durable_key, ''), goal, status, created_at, updated_at,
			`+runOriginColumns+` FROM workflow_run WHERE id = ?`,
		runID,
	).Scan(&summary.ID, &summary.DefinitionID, &summary.DefinitionVersion, &summary.DurableKey, &summary.Goal, &summary.Status, &createdAt, &updatedAt,
		&summary.Origin.PrincipalID, &summary.Origin.PrincipalClass, &summary.Origin.AgentID, &summary.Origin.SessionID, &summary.Origin.Reply.Channel, &summary.Origin.Reply.Account, &summary.Origin.Reply.ChannelID, &summary.Origin.Reply.ThreadID, &summary.Origin.Tainted, &summary.ParentRunID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, codedError(ErrorCodeRunNotFound, "run "+runID+" is not registered")
	}
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("read run: %w", err))
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
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx, query, args...)
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list runs: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var runs []*RunSummary
	for rows.Next() {
		var summary RunSummary
		var createdAt, updatedAt string
		if err := rows.Scan(&summary.ID, &summary.DefinitionID, &summary.DefinitionVersion, &summary.DurableKey, &summary.Goal, &summary.Status, &createdAt, &updatedAt); err != nil {
			return nil, sqlite.Classify(fmt.Errorf("scan run: %w", err))
		}
		summary.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		summary.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
		runs = append(runs, &summary)
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("iterate runs: %w", err))
	}
	return runs, nil
}

func (s *Store) Steps(ctx context.Context, runID string) ([]*StepRun, error) {
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx,
		`SELECT step_id, status, attempt, started_at, ended_at, COALESCE(output_json, ''), COALESCE(output_artifact_digest, ''), COALESCE(error_code, ''), updated_at, tainted, private FROM workflow_step_run WHERE run_id = ? ORDER BY step_id`,
		runID,
	)
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list steps: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var steps []*StepRun
	for rows.Next() {
		step := &StepRun{RunID: runID}
		var startedAt, endedAt, updatedAt sql.NullString
		var output string
		if err := rows.Scan(&step.StepID, &step.Status, &step.Attempt, &startedAt, &endedAt, &output, &step.OutputArtifactDigest, &step.ErrorCode, &updatedAt, &step.Tainted, &step.Private); err != nil {
			return nil, sqlite.Classify(fmt.Errorf("scan step: %w", err))
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
		return nil, sqlite.Classify(fmt.Errorf("iterate steps: %w", err))
	}
	return steps, nil
}

func (s *Store) SetRunStatus(ctx context.Context, runID, status string) error {
	result, err := sqlite.Conn(ctx, s.db).ExecContext(ctx,
		`UPDATE workflow_run SET status = ?, updated_at = ? WHERE id = ?`,
		status, time.Now().UTC().Format(time.RFC3339Nano), runID,
	)
	if err != nil {
		return sqlite.Classify(fmt.Errorf("update run status: %w", err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return sqlite.Classify(fmt.Errorf("read run update: %w", err))
	}
	if affected == 0 {
		return codedError(ErrorCodeRunNotFound, "run "+runID+" is not registered")
	}
	return nil
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
	Tainted        bool
	Private        bool
}

func (s *Store) updateStep(ctx context.Context, runID, stepID string, update *stepUpdate) error {
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
	result, err := sqlite.Conn(ctx, s.db).ExecContext(ctx,
		`UPDATE workflow_step_run SET status = ?, attempt = ?, started_at = ?, ended_at = ?, output_json = ?, output_artifact_digest = ?, error_code = ?, updated_at = ?, tainted = ?, private = ? WHERE run_id = ? AND step_id = ?`,
		update.Status, update.Attempt, format(update.StartedAt), format(update.EndedAt), output, nullIfEmpty(update.ArtifactDigest), nullIfEmpty(update.ErrorCode), time.Now().UTC().Format(time.RFC3339Nano), update.Tainted, update.Private, runID, stepID,
	)
	if err != nil {
		return sqlite.Classify(fmt.Errorf("update step %s: %w", stepID, err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return sqlite.Classify(fmt.Errorf("read step update: %w", err))
	}
	if affected == 0 {
		return codedError(ErrorCodeRunNotFound, fmt.Sprintf("run %s has no step %s", runID, stepID))
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
	err := sqlite.Conn(ctx, s.db).QueryRowContext(ctx, `SELECT input_json FROM workflow_run WHERE id = ?`, runID).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, codedError(ErrorCodeRunNotFound, "run "+runID+" is not registered")
	}
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("read run input: %w", err))
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
	COALESCE(reply_channel, ''), COALESCE(reply_account, ''), COALESCE(reply_channel_id, ''), COALESCE(reply_thread_id, ''), tainted, COALESCE(parent_run_id, '')`

const MaxCompletionAttempts = 5

func (s *Store) PendingCompletions(ctx context.Context, limit int) ([]*RunSummary, error) {
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx, `SELECT id, definition_id, definition_version, COALESCE(durable_key, ''), goal, status, created_at, updated_at, `+runOriginColumns+`
		FROM workflow_run WHERE origin_session_id IS NOT NULL AND completion_notified_at IS NULL AND parent_run_id IS NULL AND status IN (?, ?, ?)
		ORDER BY updated_at, id LIMIT ?`, RunSucceeded, RunFailed, RunCancelled, limit)
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list pending completions: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var runs []*RunSummary
	for rows.Next() {
		var summary RunSummary
		var createdAt, updatedAt string
		if err := rows.Scan(&summary.ID, &summary.DefinitionID, &summary.DefinitionVersion, &summary.DurableKey, &summary.Goal, &summary.Status, &createdAt, &updatedAt,
			&summary.Origin.PrincipalID, &summary.Origin.PrincipalClass, &summary.Origin.AgentID, &summary.Origin.SessionID, &summary.Origin.Reply.Channel, &summary.Origin.Reply.Account, &summary.Origin.Reply.ChannelID, &summary.Origin.Reply.ThreadID, &summary.Origin.Tainted, &summary.ParentRunID); err != nil {
			return nil, sqlite.Classify(fmt.Errorf("scan pending completion: %w", err))
		}
		summary.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		summary.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
		runs = append(runs, &summary)
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("iterate pending completions: %w", err))
	}
	return runs, nil
}

func (s *Store) ClaimCompletion(ctx context.Context, runID string, at time.Time) (bool, error) {
	result, err := sqlite.Conn(ctx, s.db).ExecContext(ctx, `UPDATE workflow_run SET completion_notified_at = ? WHERE id = ? AND completion_notified_at IS NULL`,
		at.UTC().Format(time.RFC3339Nano), runID)
	if err != nil {
		return false, sqlite.Classify(fmt.Errorf("claim completion: %w", err))
	}
	claimed, err := result.RowsAffected()
	if err != nil {
		return false, sqlite.Classify(fmt.Errorf("claim completion: %w", err))
	}
	return claimed == 1, nil
}

func (s *Store) ReleaseCompletion(ctx context.Context, runID string) (exhausted bool, err error) {
	var attempts int
	err = sqlite.Conn(ctx, s.db).QueryRowContext(ctx, `UPDATE workflow_run SET completion_attempts = completion_attempts + 1,
		completion_notified_at = CASE WHEN completion_attempts + 1 >= ? THEN completion_notified_at ELSE NULL END
		WHERE id = ? RETURNING completion_attempts`, MaxCompletionAttempts, runID).Scan(&attempts)
	if err != nil {
		return false, sqlite.Classify(fmt.Errorf("release completion: %w", err))
	}
	return attempts >= MaxCompletionAttempts, nil
}

func (s *Store) CompletionAttempts(ctx context.Context, runID string) (int, error) {
	var attempts int
	if err := sqlite.Conn(ctx, s.db).QueryRowContext(ctx, `SELECT completion_attempts FROM workflow_run WHERE id = ?`, runID).Scan(&attempts); err != nil {
		return 0, sqlite.Classify(fmt.Errorf("read completion attempts: %w", err))
	}
	return attempts, nil
}

func (s *Store) runStatus(ctx context.Context, runID string) (string, error) {
	var status string
	if err := sqlite.Conn(ctx, s.db).QueryRowContext(ctx, `SELECT status FROM workflow_run WHERE id = ?`, runID).Scan(&status); err != nil {
		return "", sqlite.Classify(fmt.Errorf("read run status: %w", err))
	}
	return status, nil
}

func (s *Store) LatestDefinition(ctx context.Context, id string) (*Spec, error) {
	var encoded string
	err := sqlite.Conn(ctx, s.db).QueryRowContext(ctx, `SELECT spec_json FROM workflow_definition WHERE id = ? ORDER BY version DESC LIMIT 1`, id).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, codedError(ErrorCodeDefinitionNotFound, "definition "+id+" is not registered")
	}
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("read latest definition: %w", err))
	}
	return decodeSpec(encoded)
}

func (s *Store) Routines(ctx context.Context) ([]*Spec, error) {
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx, `SELECT d.spec_json FROM workflow_definition d
		WHERE d.name IS NOT NULL AND d.source = ? AND d.version = (SELECT MAX(version) FROM workflow_definition WHERE id = d.id)
		ORDER BY d.id`, string(SourceGenerated))
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list routines: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var routines []*Spec
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			return nil, sqlite.Classify(fmt.Errorf("scan routine: %w", err))
		}
		spec, err := decodeSpec(encoded)
		if err != nil {
			return nil, err
		}
		routines = append(routines, spec)
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("iterate routines: %w", err))
	}
	return routines, nil
}

func (s *Store) RunByDurableKey(ctx context.Context, durableKey string) (*RunSummary, bool, error) {
	var runID string
	err := sqlite.Conn(ctx, s.db).QueryRowContext(ctx, `SELECT id FROM workflow_run WHERE durable_key = ?`, durableKey).Scan(&runID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, sqlite.Classify(fmt.Errorf("read run by durable key: %w", err))
	}
	run, err := s.Run(ctx, runID)
	return run, err == nil, err
}
