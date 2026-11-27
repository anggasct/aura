package child

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const selectChildRunColumns = `SELECT id, idempotency_key, parent_session_id, parent_turn_id, parent_invocation_id,
	child_session_id, durable_key, context_digest, grants_json, budget_json,
	state, deadline, created_at, updated_at,
	result_status, result_output, result_artifacts_json, tokens_used, cost_micros,
	completed_at, result_provenance,
	result_child_id, result_session_id, result_context_digest, result_durable_key,
	result_source_range, result_model, result_prompt_version, result_trust FROM child_run`

type Run struct {
	ID                  string
	IdempotencyKey      string
	ParentSessionID     string
	ParentTurnID        string
	ParentInvocation    string
	ChildSessionID      string
	DurableKey          string
	ContextDigest       string
	GrantsJSON          string
	BudgetJSON          string
	State               string
	Deadline            time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
	ResultStatus        string
	ResultOutput        string
	ResultArtifactsJSON string
	TokensUsed          int64
	CostMicros          int64
	CompletedAt         time.Time
	ResultProvenance    string
	ResultChildID       string
	ResultSessionID     string
	ResultContextDigest string
	ResultDurableKey    string
	ResultSourceRange   string
	ResultModel         string
	ResultPromptVersion string
	ResultTrust         string
}

type StoredResult struct {
	Status        string
	Output        string
	ArtifactsJSON string
	TokensUsed    int64
	CostMicros    int64
	CompletedAt   time.Time
	Provenance    string
	ChildID       string
	SessionID     string
	ContextDigest string
	DurableKey    string
	SourceRange   string
	Model         string
	PromptVersion string
	Trust         string
}

type Store interface {
	InsertRun(ctx context.Context, run *Run) error
	GetRun(ctx context.Context, id string) (Run, bool, error)
	GetRunByInvocation(ctx context.Context, parentInvocation, idempotencyKey string) (Run, bool, error)
	GetByChildSession(ctx context.Context, childSessionID string) (Run, bool, error)
	Depth(ctx context.Context, sessionID string) (int, error)
	SetState(ctx context.Context, id, state string, now time.Time) error
	SetResult(ctx context.Context, id string, result *StoredResult, now time.Time) error
	ActiveForParent(ctx context.Context, parentSessionID string) ([]Run, error)
	List(ctx context.Context, state string, limit int) ([]Run, error)
}

type SQLiteStore struct {
	db *sql.DB
}

func NewSQLiteStore(db *sql.DB) (*SQLiteStore, error) {
	if db == nil {
		return nil, errNilArgument("db")
	}
	return &SQLiteStore{db: db}, nil
}

func isConstraintUnique(err error) bool {
	classified := sqlite.Classify(err)
	return fault.HasCode(classified, sqlite.ErrorCodeConstraintUnique) ||
		fault.HasCode(classified, sqlite.ErrorCodeConstraintPrimaryKey)
}

func checkRun(run *Run) error {
	if run == nil {
		return errNilArgument("run")
	}
	if strings.TrimSpace(run.ID) == "" || strings.TrimSpace(run.IdempotencyKey) == "" {
		return Errorf(ErrorCodeInvalidArgument, "child run id and idempotency key must not be empty")
	}
	if strings.TrimSpace(run.ParentSessionID) == "" || strings.TrimSpace(run.ParentTurnID) == "" || strings.TrimSpace(run.ParentInvocation) == "" {
		return Errorf(ErrorCodeInvalidArgument, "child run lineage must not be empty")
	}
	if strings.TrimSpace(run.ChildSessionID) == "" || strings.TrimSpace(run.DurableKey) == "" {
		return Errorf(ErrorCodeInvalidArgument, "child session and durable key must not be empty")
	}
	if strings.TrimSpace(run.ContextDigest) == "" {
		return Errorf(ErrorCodeInvalidArgument, "child context digest must not be empty")
	}
	if !json.Valid([]byte(run.GrantsJSON)) || !json.Valid([]byte(run.BudgetJSON)) {
		return Errorf(ErrorCodeInvalidArgument, "child grants and budget must be valid JSON")
	}
	if !ValidState(run.State) {
		return Errorf(ErrorCodeInvalidArgument, "child run state is not valid")
	}
	if run.Deadline.IsZero() || run.CreatedAt.IsZero() || run.UpdatedAt.IsZero() {
		return Errorf(ErrorCodeInvalidArgument, "child run timestamps must not be zero")
	}
	return nil
}

func (s *SQLiteStore) InsertRun(ctx context.Context, run *Run) error {
	if s == nil || s.db == nil {
		return errNilArgument("db")
	}
	if err := checkRun(run); err != nil {
		return err
	}
	_, err := sqlite.Conn(ctx, s.db).ExecContext(ctx,
		`INSERT INTO child_run (
			id, idempotency_key, parent_session_id, parent_turn_id, parent_invocation_id,
			child_session_id, durable_key, context_digest, grants_json, budget_json,
			state, deadline, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ID, run.IdempotencyKey, run.ParentSessionID, run.ParentTurnID, run.ParentInvocation,
		run.ChildSessionID, run.DurableKey, run.ContextDigest, run.GrantsJSON, run.BudgetJSON,
		run.State, sqlite.FormatTime(run.Deadline.UTC()), sqlite.FormatTime(run.CreatedAt.UTC()), sqlite.FormatTime(run.UpdatedAt.UTC()),
	)
	if err != nil {
		if isConstraintUnique(err) {
			return Errorf(ErrorCodeChildConflict, "child run already exists")
		}
		return sqlite.Classify(fmt.Errorf("insert child run: %w", err))
	}
	return nil
}

func scanRun(rows *sql.Rows) (Run, error) {
	var run Run
	var deadline, created, updated string
	var resultStatus, resultOutput, completedAt, resultProvenance sql.NullString
	var resultChildID, resultSessionID, resultContextDigest, resultDurableKey sql.NullString
	var resultSourceRange, resultModel, resultPromptVersion, resultTrust sql.NullString
	if err := rows.Scan(
		&run.ID, &run.IdempotencyKey, &run.ParentSessionID, &run.ParentTurnID, &run.ParentInvocation,
		&run.ChildSessionID, &run.DurableKey, &run.ContextDigest, &run.GrantsJSON, &run.BudgetJSON,
		&run.State, &deadline, &created, &updated,
		&resultStatus, &resultOutput, &run.ResultArtifactsJSON, &run.TokensUsed, &run.CostMicros,
		&completedAt, &resultProvenance,
		&resultChildID, &resultSessionID, &resultContextDigest, &resultDurableKey,
		&resultSourceRange, &resultModel, &resultPromptVersion, &resultTrust,
	); err != nil {
		return Run{}, sqlite.Classify(fmt.Errorf("scan child run: %w", err))
	}
	for target, raw := range map[*time.Time]string{&run.Deadline: deadline, &run.CreatedAt: created, &run.UpdatedAt: updated} {
		parsed, err := sqlite.ParseTime(raw)
		if err != nil {
			return Run{}, fmt.Errorf("parse child run timestamps: %w", err)
		}
		*target = parsed
	}
	if resultStatus.Valid {
		run.ResultStatus = resultStatus.String
	}
	if resultOutput.Valid {
		run.ResultOutput = resultOutput.String
	}
	if completedAt.Valid && completedAt.String != "" {
		parsed, err := sqlite.ParseTime(completedAt.String)
		if err != nil {
			return Run{}, fmt.Errorf("parse child result timestamp: %w", err)
		}
		run.CompletedAt = parsed
	}
	if resultProvenance.Valid {
		run.ResultProvenance = resultProvenance.String
	}
	if resultChildID.Valid {
		run.ResultChildID = resultChildID.String
	}
	if resultSessionID.Valid {
		run.ResultSessionID = resultSessionID.String
	}
	if resultContextDigest.Valid {
		run.ResultContextDigest = resultContextDigest.String
	}
	if resultDurableKey.Valid {
		run.ResultDurableKey = resultDurableKey.String
	}
	if resultSourceRange.Valid {
		run.ResultSourceRange = resultSourceRange.String
	}
	if resultModel.Valid {
		run.ResultModel = resultModel.String
	}
	if resultPromptVersion.Valid {
		run.ResultPromptVersion = resultPromptVersion.String
	}
	if resultTrust.Valid {
		run.ResultTrust = resultTrust.String
	}
	if run.ResultArtifactsJSON == "" {
		run.ResultArtifactsJSON = "[]"
	}
	return run, nil
}

func (s *SQLiteStore) queryOneRun(ctx context.Context, op, query string, args ...any) (Run, bool, error) {
	if s == nil || s.db == nil {
		return Run{}, false, errNilArgument("db")
	}
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx, query, args...)
	if err != nil {
		return Run{}, false, sqlite.Classify(fmt.Errorf("%s: %w", op, err))
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Run{}, false, sqlite.Classify(fmt.Errorf("%s: %w", op, err))
		}
		return Run{}, false, nil
	}
	run, err := scanRun(rows)
	if err != nil {
		return Run{}, false, err
	}
	if err := rows.Err(); err != nil {
		return Run{}, false, sqlite.Classify(fmt.Errorf("%s: %w", op, err))
	}
	return run, true, nil
}

func (s *SQLiteStore) GetRun(ctx context.Context, id string) (Run, bool, error) {
	return s.queryOneRun(ctx, "get child run", selectChildRunColumns+` WHERE id = ?`, id)
}

func (s *SQLiteStore) GetRunByInvocation(ctx context.Context, parentInvocation, idempotencyKey string) (Run, bool, error) {
	return s.queryOneRun(ctx, "get child run by invocation", selectChildRunColumns+` WHERE parent_invocation_id = ? AND idempotency_key = ?`, parentInvocation, idempotencyKey)
}

func (s *SQLiteStore) GetByChildSession(ctx context.Context, childSessionID string) (Run, bool, error) {
	return s.queryOneRun(ctx, "get child run by session", selectChildRunColumns+` WHERE child_session_id = ? LIMIT 1`, childSessionID)
}

func (s *SQLiteStore) Depth(ctx context.Context, sessionID string) (int, error) {
	if s == nil || s.db == nil {
		return 0, errNilArgument("db")
	}
	if strings.TrimSpace(sessionID) == "" {
		return 0, Errorf(ErrorCodeInvalidArgument, "child session must not be empty")
	}
	var one int
	err := sqlite.Conn(ctx, s.db).QueryRowContext(ctx,
		`SELECT 1 FROM child_run WHERE child_session_id = ? LIMIT 1`, sessionID,
	).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, sqlite.Classify(fmt.Errorf("query child depth: %w", err))
	}
	return 1, nil
}

func (s *SQLiteStore) SetState(ctx context.Context, id, state string, now time.Time) error {
	if s == nil || s.db == nil {
		return errNilArgument("db")
	}
	if !ValidState(state) {
		return Errorf(ErrorCodeInvalidArgument, "child run state is not valid")
	}
	result, err := sqlite.Conn(ctx, s.db).ExecContext(ctx,
		`UPDATE child_run SET state = ?, updated_at = ? WHERE id = ?`,
		state, sqlite.FormatTime(now.UTC()), id,
	)
	if err != nil {
		return sqlite.Classify(fmt.Errorf("set child run state: %w", err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return sqlite.Classify(fmt.Errorf("set child run state: %w", err))
	}
	if affected == 0 {
		return Errorf(ErrorCodeChildNotFound, "child run is not found")
	}
	return nil
}

func (s *SQLiteStore) SetResult(ctx context.Context, id string, result *StoredResult, now time.Time) error {
	if s == nil || s.db == nil {
		return errNilArgument("db")
	}
	if result == nil {
		return Errorf(ErrorCodeInvalidArgument, "child result must not be nil")
	}
	if strings.TrimSpace(result.Status) == "" {
		return Errorf(ErrorCodeInvalidArgument, "child result status must not be empty")
	}
	if result.TokensUsed < 0 || result.CostMicros < 0 {
		return Errorf(ErrorCodeInvalidArgument, "child result usage must not be negative")
	}
	if len(result.Output) > 8192 {
		return Errorf(ErrorCodeInvalidArgument, "child result output exceeds the bound")
	}
	artifactsJSON := result.ArtifactsJSON
	if strings.TrimSpace(artifactsJSON) == "" {
		artifactsJSON = "[]"
	}
	if !json.Valid([]byte(artifactsJSON)) {
		return Errorf(ErrorCodeInvalidArgument, "child result artifacts must be valid JSON")
	}
	var artifacts []string
	if err := json.Unmarshal([]byte(artifactsJSON), &artifacts); err != nil {
		return Errorf(ErrorCodeInvalidArgument, "child result artifacts must decode")
	}
	if len(artifacts) > 32 {
		return Errorf(ErrorCodeInvalidArgument, "child result artifacts exceed the bound")
	}
	completedAt := result.CompletedAt
	if completedAt.IsZero() {
		completedAt = now.UTC()
	}
	var completedRaw any
	if !completedAt.IsZero() {
		completedRaw = sqlite.FormatTime(completedAt.UTC())
	}
	var provenanceRaw any
	if strings.TrimSpace(result.Provenance) != "" {
		provenanceRaw = result.Provenance
	}
	if strings.TrimSpace(result.Trust) != "" {
		switch result.Trust {
		case "derived_untrusted", "untrusted_external":
		default:
			return Errorf(ErrorCodeInvalidArgument, "child result trust must be untrusted")
		}
	}
	if len(result.Model) > 128 || len(result.PromptVersion) > 64 || len(result.SourceRange) > 512 {
		return Errorf(ErrorCodeInvalidArgument, "child result identity fields exceed the bound")
	}
	nullable := func(v string) any {
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return v
	}
	res, err := sqlite.Conn(ctx, s.db).ExecContext(ctx,
		`UPDATE child_run SET result_status = ?, result_output = ?, result_artifacts_json = ?,
			tokens_used = ?, cost_micros = ?, completed_at = ?, result_provenance = ?,
			result_child_id = ?, result_session_id = ?, result_context_digest = ?, result_durable_key = ?,
			result_source_range = ?, result_model = ?, result_prompt_version = ?, result_trust = ?,
			updated_at = ?
			WHERE id = ?`,
		result.Status, result.Output, artifactsJSON,
		result.TokensUsed, result.CostMicros, completedRaw, provenanceRaw,
		nullable(result.ChildID), nullable(result.SessionID), nullable(result.ContextDigest), nullable(result.DurableKey),
		nullable(result.SourceRange), nullable(result.Model), nullable(result.PromptVersion), nullable(result.Trust),
		sqlite.FormatTime(now.UTC()), id,
	)
	if err != nil {
		return sqlite.Classify(fmt.Errorf("set child result: %w", err))
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return sqlite.Classify(fmt.Errorf("set child result: %w", err))
	}
	if affected == 0 {
		return Errorf(ErrorCodeChildNotFound, "child run is not found")
	}
	return nil
}

func (s *SQLiteStore) ActiveForParent(ctx context.Context, parentSessionID string) ([]Run, error) {
	if s == nil || s.db == nil {
		return nil, errNilArgument("db")
	}
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx,
		selectChildRunColumns+` WHERE parent_session_id = ? AND state IN ('queued','running') ORDER BY created_at, id`,
		parentSessionID,
	)
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list active child runs: %w", err))
	}
	defer func() { _ = rows.Close() }()
	runs := []Run{}
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list active child runs: %w", err))
	}
	return runs, nil
}

func (s *SQLiteStore) List(ctx context.Context, state string, limit int) ([]Run, error) {
	if s == nil || s.db == nil {
		return nil, errNilArgument("db")
	}
	if limit <= 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "child list limit must be positive")
	}
	query := selectChildRunColumns
	args := []any{}
	if state != "" {
		if !ValidState(state) {
			return nil, Errorf(ErrorCodeInvalidArgument, "child run state is not valid")
		}
		query += ` WHERE state = ?`
		args = append(args, state)
	}
	query += ` ORDER BY created_at, id LIMIT ?`
	args = append(args, limit)
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx, query, args...)
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list child runs: %w", err))
	}
	defer func() { _ = rows.Close() }()
	runs := []Run{}
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list child runs: %w", err))
	}
	return runs, nil
}
