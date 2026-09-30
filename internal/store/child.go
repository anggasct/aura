package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type ChildRun struct {
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

type ChildResult struct {
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

type ChildStore interface {
	InsertRun(ctx context.Context, run *ChildRun) error
	GetRun(ctx context.Context, id string) (ChildRun, bool, error)
	GetRunByInvocation(ctx context.Context, parentInvocation, idempotencyKey string) (ChildRun, bool, error)
	SetState(ctx context.Context, id, state string, now time.Time) error
	SetResult(ctx context.Context, id string, result *ChildResult, now time.Time) error
	ActiveForParent(ctx context.Context, parentSessionID string) ([]ChildRun, error)
	List(ctx context.Context, state string, limit int) ([]ChildRun, error)
}

type sqliteChildStore struct {
	db *sql.DB
}

func NewChildStore(db *sql.DB) ChildStore {
	return &sqliteChildStore{db: db}
}

func checkChildRun(run *ChildRun) error {
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
	switch run.State {
	case "queued", "running", "succeeded", "failed", "cancelled", "deadline_exceeded", "interrupted":
	default:
		return Errorf(ErrorCodeInvalidArgument, "child run state is not valid")
	}
	if run.Deadline.IsZero() || run.CreatedAt.IsZero() || run.UpdatedAt.IsZero() {
		return Errorf(ErrorCodeInvalidArgument, "child run timestamps must not be zero")
	}
	return nil
}

func (s *sqliteChildStore) InsertRun(ctx context.Context, run *ChildRun) error {
	if s.db == nil {
		return errNilArgument("db")
	}
	if err := checkChildRun(run); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO child_run (
			id, idempotency_key, parent_session_id, parent_turn_id, parent_invocation_id,
			child_session_id, durable_key, context_digest, grants_json, budget_json,
			state, deadline, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ID, run.IdempotencyKey, run.ParentSessionID, run.ParentTurnID, run.ParentInvocation,
		run.ChildSessionID, run.DurableKey, run.ContextDigest, run.GrantsJSON, run.BudgetJSON,
		run.State, formatTime(run.Deadline.UTC()), formatTime(run.CreatedAt.UTC()), formatTime(run.UpdatedAt.UTC()),
	)
	if err != nil {
		if isConstraintUnique(err) {
			return Errorf(ErrorCodeChildConflict, "child run already exists")
		}
		return classifyBusy(fmt.Errorf("insert child run: %w", err))
	}
	return nil
}

func scanChildRun(rows *sql.Rows) (ChildRun, error) {
	var run ChildRun
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
		return ChildRun{}, fmt.Errorf("scan child run: %w", err)
	}
	for target, raw := range map[*time.Time]string{&run.Deadline: deadline, &run.CreatedAt: created, &run.UpdatedAt: updated} {
		parsed, err := parseTime(raw)
		if err != nil {
			return ChildRun{}, fmt.Errorf("parse child run timestamps: %w", err)
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
		parsed, err := parseTime(completedAt.String)
		if err != nil {
			return ChildRun{}, fmt.Errorf("parse child result timestamp: %w", err)
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

func (s *sqliteChildStore) GetRun(ctx context.Context, id string) (ChildRun, bool, error) {
	if s.db == nil {
		return ChildRun{}, false, errNilArgument("db")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, idempotency_key, parent_session_id, parent_turn_id, parent_invocation_id,
			child_session_id, durable_key, context_digest, grants_json, budget_json,
			state, deadline, created_at, updated_at,
			result_status, result_output, result_artifacts_json, tokens_used, cost_micros,
			completed_at, result_provenance,
			result_child_id, result_session_id, result_context_digest, result_durable_key,
			result_source_range, result_model, result_prompt_version, result_trust FROM child_run WHERE id = ?`, id,
	)
	if err != nil {
		return ChildRun{}, false, classifyBusy(fmt.Errorf("get child run: %w", err))
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return ChildRun{}, false, nil
	}
	run, err := scanChildRun(rows)
	if err != nil {
		return ChildRun{}, false, err
	}
	return run, true, nil
}

func (s *sqliteChildStore) GetRunByInvocation(ctx context.Context, parentInvocation, idempotencyKey string) (ChildRun, bool, error) {
	if s.db == nil {
		return ChildRun{}, false, errNilArgument("db")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, idempotency_key, parent_session_id, parent_turn_id, parent_invocation_id,
			child_session_id, durable_key, context_digest, grants_json, budget_json,
			state, deadline, created_at, updated_at,
			result_status, result_output, result_artifacts_json, tokens_used, cost_micros,
			completed_at, result_provenance,
			result_child_id, result_session_id, result_context_digest, result_durable_key,
			result_source_range, result_model, result_prompt_version, result_trust FROM child_run
		 WHERE parent_invocation_id = ? AND idempotency_key = ?`, parentInvocation, idempotencyKey,
	)
	if err != nil {
		return ChildRun{}, false, classifyBusy(fmt.Errorf("get child run by invocation: %w", err))
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return ChildRun{}, false, nil
	}
	run, err := scanChildRun(rows)
	if err != nil {
		return ChildRun{}, false, err
	}
	return run, true, nil
}

func (s *sqliteChildStore) SetState(ctx context.Context, id, state string, now time.Time) error {
	if s.db == nil {
		return errNilArgument("db")
	}
	switch state {
	case "queued", "running", "succeeded", "failed", "cancelled", "deadline_exceeded", "interrupted":
	default:
		return Errorf(ErrorCodeInvalidArgument, "child run state is not valid")
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE child_run SET state = ?, updated_at = ? WHERE id = ?`,
		state, formatTime(now.UTC()), id,
	)
	if err != nil {
		return classifyBusy(fmt.Errorf("set child run state: %w", err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("set child run state: %w", err)
	}
	if affected == 0 {
		return Errorf(ErrorCodeChildNotFound, "child run is not found")
	}
	return nil
}

func (s *sqliteChildStore) SetResult(ctx context.Context, id string, result *ChildResult, now time.Time) error {
	if s.db == nil {
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
		completedRaw = formatTime(completedAt.UTC())
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
	res, err := s.db.ExecContext(ctx,
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
		formatTime(now.UTC()), id,
	)
	if err != nil {
		return classifyBusy(fmt.Errorf("set child result: %w", err))
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set child result: %w", err)
	}
	if affected == 0 {
		return Errorf(ErrorCodeChildNotFound, "child run is not found")
	}
	return nil
}

func (s *sqliteChildStore) ActiveForParent(ctx context.Context, parentSessionID string) ([]ChildRun, error) {
	if s.db == nil {
		return nil, errNilArgument("db")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, idempotency_key, parent_session_id, parent_turn_id, parent_invocation_id,
			child_session_id, durable_key, context_digest, grants_json, budget_json,
			state, deadline, created_at, updated_at,
			result_status, result_output, result_artifacts_json, tokens_used, cost_micros,
			completed_at, result_provenance,
			result_child_id, result_session_id, result_context_digest, result_durable_key,
			result_source_range, result_model, result_prompt_version, result_trust FROM child_run
		 WHERE parent_session_id = ? AND state IN ('queued','running')
		 ORDER BY created_at, id`, parentSessionID,
	)
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("list active child runs: %w", err))
	}
	defer func() { _ = rows.Close() }()
	runs := []ChildRun{}
	for rows.Next() {
		run, err := scanChildRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list active child runs: %w", err)
	}
	return runs, nil
}

func (s *sqliteChildStore) List(ctx context.Context, state string, limit int) ([]ChildRun, error) {
	if s.db == nil {
		return nil, errNilArgument("db")
	}
	if limit <= 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "child list limit must be positive")
	}
	query := `SELECT id, idempotency_key, parent_session_id, parent_turn_id, parent_invocation_id,
		child_session_id, durable_key, context_digest, grants_json, budget_json,
		state, deadline, created_at, updated_at,
		result_status, result_output, result_artifacts_json, tokens_used, cost_micros,
		completed_at, result_provenance,
		result_child_id, result_session_id, result_context_digest, result_durable_key,
		result_source_range, result_model, result_prompt_version, result_trust FROM child_run`
	args := []any{}
	if state != "" {
		switch state {
		case "queued", "running", "succeeded", "failed", "cancelled", "deadline_exceeded", "interrupted":
		default:
			return nil, Errorf(ErrorCodeInvalidArgument, "child run state is not valid")
		}
		query += ` WHERE state = ?`
		args = append(args, state)
	}
	query += ` ORDER BY created_at, id LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("list child runs: %w", err))
	}
	defer func() { _ = rows.Close() }()
	runs := []ChildRun{}
	for rows.Next() {
		run, err := scanChildRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list child runs: %w", err)
	}
	return runs, nil
}
