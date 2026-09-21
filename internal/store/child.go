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
	ID               string
	IdempotencyKey   string
	ParentSessionID  string
	ParentTurnID     string
	ParentInvocation string
	ChildSessionID   string
	Depth            int
	DurableKey       string
	ContextDigest    string
	GrantsJSON       string
	BudgetJSON       string
	State            string
	Deadline         time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type ChildStore interface {
	InsertRun(ctx context.Context, run *ChildRun) error
	GetRun(ctx context.Context, id string) (ChildRun, bool, error)
	GetRunByInvocation(ctx context.Context, parentInvocation, idempotencyKey string) (ChildRun, bool, error)
	SetState(ctx context.Context, id, state string, now time.Time) error
	ActiveForParent(ctx context.Context, parentSessionID string) ([]ChildRun, error)
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
	if run.Depth != 1 {
		return Errorf(ErrorCodeInvalidArgument, "child run depth must be one")
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
			child_session_id, depth, durable_key, context_digest, grants_json, budget_json,
			state, deadline, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ID, run.IdempotencyKey, run.ParentSessionID, run.ParentTurnID, run.ParentInvocation,
		run.ChildSessionID, run.Depth, run.DurableKey, run.ContextDigest, run.GrantsJSON, run.BudgetJSON,
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
	if err := rows.Scan(
		&run.ID, &run.IdempotencyKey, &run.ParentSessionID, &run.ParentTurnID, &run.ParentInvocation,
		&run.ChildSessionID, &run.Depth, &run.DurableKey, &run.ContextDigest, &run.GrantsJSON, &run.BudgetJSON,
		&run.State, &deadline, &created, &updated,
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
	return run, nil
}

func (s *sqliteChildStore) GetRun(ctx context.Context, id string) (ChildRun, bool, error) {
	if s.db == nil {
		return ChildRun{}, false, errNilArgument("db")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, idempotency_key, parent_session_id, parent_turn_id, parent_invocation_id,
			child_session_id, depth, durable_key, context_digest, grants_json, budget_json,
			state, deadline, created_at, updated_at FROM child_run WHERE id = ?`, id,
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
			child_session_id, depth, durable_key, context_digest, grants_json, budget_json,
			state, deadline, created_at, updated_at FROM child_run
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

func (s *sqliteChildStore) ActiveForParent(ctx context.Context, parentSessionID string) ([]ChildRun, error) {
	if s.db == nil {
		return nil, errNilArgument("db")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, idempotency_key, parent_session_id, parent_turn_id, parent_invocation_id,
			child_session_id, depth, durable_key, context_digest, grants_json, budget_json,
			state, deadline, created_at, updated_at FROM child_run
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
