package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	WebhookExecutionStateAccepted  = "accepted"
	WebhookExecutionStateRunning   = "running"
	WebhookExecutionStateCompleted = "completed"
	WebhookExecutionStateFailed    = "failed"
	WebhookExecutionStateCancelled = "cancelled"
)

type WebhookExecution struct {
	ID            string
	KeyID         string
	EventID       string
	Nonce         string
	BodyDigest    string
	TurnID        string
	SessionID     string
	State         string
	ResultEventID string
	ErrorCode     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	ExpiresAt     time.Time
}

type WebhookExecutionStore interface {
	InsertExecution(ctx context.Context, execution *WebhookExecution) error
	Execution(ctx context.Context, id string) (WebhookExecution, error)
	ExecutionByNonce(ctx context.Context, keyID, nonce string) (WebhookExecution, bool, error)
	ExecutionByEvent(ctx context.Context, keyID, eventID string) (WebhookExecution, bool, error)
	MarkRunning(ctx context.Context, id string, updatedAt time.Time) (bool, error)
	MarkTerminal(ctx context.Context, id, state, resultEventID, errorCode string, updatedAt time.Time) (bool, error)
	DeleteExecution(ctx context.Context, id string) error
	ListUnfinished(ctx context.Context, query *UnfinishedExecutionQuery) ([]WebhookExecution, error)
}

type UnfinishedExecutionQuery struct {
	Now     time.Time
	AfterID string
	Limit   int
}

type sqliteWebhookExecutionStore struct {
	db *sql.DB
}

func NewWebhookExecutionStore(db *sql.DB) WebhookExecutionStore {
	return &sqliteWebhookExecutionStore{db: db}
}

func (s *sqliteWebhookExecutionStore) InsertExecution(ctx context.Context, execution *WebhookExecution) error {
	if s.db == nil {
		return errNilArgument("db")
	}
	if execution == nil {
		return errNilArgument("execution")
	}
	if execution.ID == "" || execution.KeyID == "" || execution.EventID == "" || execution.Nonce == "" || execution.TurnID == "" || execution.SessionID == "" {
		return Errorf(ErrorCodeInvalidArgument, "execution identity must not be empty")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO webhook_execution (
			id, key_id, event_id, nonce, body_digest, turn_id, session_id, state,
			result_event_id, error_code, created_at, updated_at, expires_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		execution.ID,
		execution.KeyID,
		execution.EventID,
		execution.Nonce,
		execution.BodyDigest,
		execution.TurnID,
		execution.SessionID,
		execution.State,
		nullText(execution.ResultEventID),
		nullText(execution.ErrorCode),
		formatTime(execution.CreatedAt.UTC()),
		formatTime(execution.UpdatedAt.UTC()),
		formatTime(execution.ExpiresAt.UTC()),
	)
	if err != nil {
		if isConstraintUnique(err) {
			return &Error{
				Code:   ErrorCodeWebhookExecutionConflict,
				Detail: fmt.Sprintf("execution identity for key %q is already claimed", execution.KeyID),
			}
		}
		return classifyBusy(fmt.Errorf("insert webhook execution: %w", err))
	}
	return nil
}

func (s *sqliteWebhookExecutionStore) Execution(ctx context.Context, id string) (WebhookExecution, error) {
	if s.db == nil {
		return WebhookExecution{}, errNilArgument("db")
	}
	execution, found, err := s.findExecution(ctx,
		`SELECT `+selectWebhookExecutionColumns+` FROM webhook_execution WHERE id = ?`, id)
	if err != nil {
		return WebhookExecution{}, err
	}
	if !found {
		return WebhookExecution{}, &Error{
			Code:   ErrorCodeWebhookExecutionNotFound,
			Detail: "webhook execution not found",
		}
	}
	return execution, nil
}

func (s *sqliteWebhookExecutionStore) ExecutionByNonce(ctx context.Context, keyID, nonce string) (WebhookExecution, bool, error) {
	if s.db == nil {
		return WebhookExecution{}, false, errNilArgument("db")
	}
	return s.findExecution(ctx,
		`SELECT `+selectWebhookExecutionColumns+` FROM webhook_execution WHERE key_id = ? AND nonce = ?`,
		keyID, nonce)
}

func (s *sqliteWebhookExecutionStore) ExecutionByEvent(ctx context.Context, keyID, eventID string) (WebhookExecution, bool, error) {
	if s.db == nil {
		return WebhookExecution{}, false, errNilArgument("db")
	}
	return s.findExecution(ctx,
		`SELECT `+selectWebhookExecutionColumns+` FROM webhook_execution WHERE key_id = ? AND event_id = ?`,
		keyID, eventID)
}

func (s *sqliteWebhookExecutionStore) MarkRunning(ctx context.Context, id string, updatedAt time.Time) (bool, error) {
	if s.db == nil {
		return false, errNilArgument("db")
	}
	if id == "" {
		return false, Errorf(ErrorCodeInvalidArgument, "transition needs an id")
	}
	return s.advanceState(ctx,
		`UPDATE webhook_execution
		 SET state = ?, updated_at = ?
		 WHERE id = ? AND state = 'accepted'`,
		WebhookExecutionStateRunning, formatTime(updatedAt.UTC()), id)
}

func (s *sqliteWebhookExecutionStore) MarkTerminal(ctx context.Context, id, state, resultEventID, errorCode string, updatedAt time.Time) (bool, error) {
	if s.db == nil {
		return false, errNilArgument("db")
	}
	switch state {
	case WebhookExecutionStateCompleted, WebhookExecutionStateFailed, WebhookExecutionStateCancelled:
	default:
		return false, Errorf(ErrorCodeInvalidArgument, "terminal state must be completed, failed, or cancelled")
	}
	return s.advanceState(ctx,
		`UPDATE webhook_execution
		 SET state = ?, result_event_id = ?, error_code = ?, updated_at = ?
		 WHERE id = ? AND state IN ('accepted','running')`,
		state, nullText(resultEventID), nullText(errorCode), formatTime(updatedAt.UTC()), id)
}

func (s *sqliteWebhookExecutionStore) advanceState(ctx context.Context, query string, args ...any) (bool, error) {
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return false, classifyBusy(fmt.Errorf("transition webhook execution: %w", err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, classifyBusy(fmt.Errorf("transition webhook execution count: %w", err))
	}
	return affected > 0, nil
}

func (s *sqliteWebhookExecutionStore) DeleteExecution(ctx context.Context, id string) error {
	if s.db == nil {
		return errNilArgument("db")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM webhook_execution WHERE id = ?`, id); err != nil {
		return classifyBusy(fmt.Errorf("delete webhook execution: %w", err))
	}
	return nil
}

func (s *sqliteWebhookExecutionStore) ListUnfinished(ctx context.Context, query *UnfinishedExecutionQuery) ([]WebhookExecution, error) {
	if s.db == nil {
		return nil, errNilArgument("db")
	}
	if query == nil {
		return nil, errNilArgument("query")
	}
	if query.Limit <= 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "unfinished execution limit must be positive")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+selectWebhookExecutionColumns+` FROM webhook_execution
		 WHERE state IN ('accepted','running') AND expires_at > ? AND id > ?
		 ORDER BY id LIMIT ?`,
		formatTime(query.Now.UTC()), query.AfterID, query.Limit)
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("list unfinished webhook executions: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var executions []WebhookExecution
	for rows.Next() {
		execution, err := scanWebhookExecution(rows)
		if err != nil {
			return nil, err
		}
		executions = append(executions, execution)
	}
	if err := rows.Err(); err != nil {
		return nil, classifyBusy(fmt.Errorf("list unfinished webhook executions: %w", err))
	}
	return executions, nil
}

const selectWebhookExecutionColumns = `id, key_id, event_id, nonce, body_digest, turn_id, session_id, state,
	result_event_id, error_code, created_at, updated_at, expires_at`

func (s *sqliteWebhookExecutionStore) findExecution(ctx context.Context, query string, args ...any) (WebhookExecution, bool, error) {
	execution, err := scanWebhookExecution(s.db.QueryRowContext(ctx, query, args...))
	switch {
	case err == nil:
		return execution, true, nil
	case errors.Is(err, sql.ErrNoRows):
		return WebhookExecution{}, false, nil
	default:
		return WebhookExecution{}, false, err
	}
}

func scanWebhookExecution(row rowScanner) (WebhookExecution, error) {
	var execution WebhookExecution
	var resultEventID, errorCode sql.NullString
	var createdAtRaw, updatedAtRaw, expiresAtRaw string
	err := row.Scan(
		&execution.ID,
		&execution.KeyID,
		&execution.EventID,
		&execution.Nonce,
		&execution.BodyDigest,
		&execution.TurnID,
		&execution.SessionID,
		&execution.State,
		&resultEventID,
		&errorCode,
		&createdAtRaw,
		&updatedAtRaw,
		&expiresAtRaw,
	)
	switch {
	case err == nil:
	case errors.Is(err, sql.ErrNoRows):
		return WebhookExecution{}, err
	default:
		return WebhookExecution{}, classifyBusy(fmt.Errorf("read webhook execution: %w", err))
	}
	if resultEventID.Valid {
		execution.ResultEventID = resultEventID.String
	}
	if errorCode.Valid {
		execution.ErrorCode = errorCode.String
	}
	var parseErr error
	if execution.CreatedAt, parseErr = parseTime(createdAtRaw); parseErr != nil {
		return WebhookExecution{}, &Error{
			Code:   ErrorCodeWebhookExecutionInvalid,
			Detail: "stored creation timestamp is not a valid time",
		}
	}
	if execution.UpdatedAt, parseErr = parseTime(updatedAtRaw); parseErr != nil {
		return WebhookExecution{}, &Error{
			Code:   ErrorCodeWebhookExecutionInvalid,
			Detail: "stored update timestamp is not a valid time",
		}
	}
	if execution.ExpiresAt, parseErr = parseTime(expiresAtRaw); parseErr != nil {
		return WebhookExecution{}, &Error{
			Code:   ErrorCodeWebhookExecutionInvalid,
			Detail: "stored expiry timestamp is not a valid time",
		}
	}
	return execution, nil
}

func nullText(value string) sql.NullString {
	if value == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: value, Valid: true}
}
