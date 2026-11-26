package webhook

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const (
	StateAccepted  = "accepted"
	StateRunning   = "running"
	StateCompleted = "completed"
	StateFailed    = "failed"
	StateCancelled = "cancelled"
)

type Execution struct {
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

type UnfinishedQuery struct {
	Now     time.Time
	AfterID string
	Limit   int
}

type ExecutionStore interface {
	InsertExecution(ctx context.Context, execution *Execution) error
	Execution(ctx context.Context, id string) (Execution, error)
	ExecutionByNonce(ctx context.Context, keyID, nonce string) (Execution, bool, error)
	ExecutionByEvent(ctx context.Context, keyID, eventID string) (Execution, bool, error)
	MarkRunning(ctx context.Context, id string, updatedAt time.Time) (bool, error)
	MarkTerminal(ctx context.Context, id, state, resultEventID, errorCode string, updatedAt time.Time) (bool, error)
	DeleteExecution(ctx context.Context, id string) error
	ListUnfinished(ctx context.Context, query *UnfinishedQuery) ([]Execution, error)
}

type rowScanner interface {
	Scan(dest ...any) error
}

type SQLiteStore struct {
	db *sql.DB
}

func NewSQLiteStore(db *sql.DB) (*SQLiteStore, error) {
	if db == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "db must not be nil")
	}
	return &SQLiteStore{db: db}, nil
}

func (s *SQLiteStore) InsertExecution(ctx context.Context, execution *Execution) error {
	if s == nil || s.db == nil {
		return Errorf(ErrorCodeInvalidArgument, "db must not be nil")
	}
	if execution == nil {
		return Errorf(ErrorCodeInvalidArgument, "execution must not be nil")
	}
	if execution.ID == "" || execution.KeyID == "" || execution.EventID == "" || execution.Nonce == "" || execution.TurnID == "" || execution.SessionID == "" {
		return Errorf(ErrorCodeInvalidArgument, "execution identity must not be empty")
	}
	_, err := sqlite.Conn(ctx, s.db).ExecContext(ctx,
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
		sqlite.FormatTime(execution.CreatedAt.UTC()),
		sqlite.FormatTime(execution.UpdatedAt.UTC()),
		sqlite.FormatTime(execution.ExpiresAt.UTC()),
	)
	if err != nil {
		classified := sqlite.Classify(err)
		if fault.HasCode(classified, sqlite.ErrorCodeConstraintUnique) || fault.HasCode(classified, sqlite.ErrorCodeConstraintPrimaryKey) {
			return Errorf(ErrorCodeExecutionConflict, "execution identity for key %q is already claimed", execution.KeyID)
		}
		return sqlite.Classify(fmt.Errorf("insert webhook execution: %w", err))
	}
	return nil
}

func (s *SQLiteStore) Execution(ctx context.Context, id string) (Execution, error) {
	if s == nil || s.db == nil {
		return Execution{}, Errorf(ErrorCodeInvalidArgument, "db must not be nil")
	}
	execution, found, err := s.findExecution(ctx,
		`SELECT `+selectExecutionColumns+` FROM webhook_execution WHERE id = ?`, id)
	if err != nil {
		return Execution{}, err
	}
	if !found {
		return Execution{}, Errorf(ErrorCodeExecutionNotFound, "webhook execution not found")
	}
	return execution, nil
}

func (s *SQLiteStore) ExecutionByNonce(ctx context.Context, keyID, nonce string) (Execution, bool, error) {
	if s == nil || s.db == nil {
		return Execution{}, false, Errorf(ErrorCodeInvalidArgument, "db must not be nil")
	}
	return s.findExecution(ctx,
		`SELECT `+selectExecutionColumns+` FROM webhook_execution WHERE key_id = ? AND nonce = ?`,
		keyID, nonce)
}

func (s *SQLiteStore) ExecutionByEvent(ctx context.Context, keyID, eventID string) (Execution, bool, error) {
	if s == nil || s.db == nil {
		return Execution{}, false, Errorf(ErrorCodeInvalidArgument, "db must not be nil")
	}
	return s.findExecution(ctx,
		`SELECT `+selectExecutionColumns+` FROM webhook_execution WHERE key_id = ? AND event_id = ?`,
		keyID, eventID)
}

func (s *SQLiteStore) MarkRunning(ctx context.Context, id string, updatedAt time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, Errorf(ErrorCodeInvalidArgument, "db must not be nil")
	}
	if id == "" {
		return false, Errorf(ErrorCodeInvalidArgument, "transition needs an id")
	}
	return s.advanceState(ctx,
		`UPDATE webhook_execution
		 SET state = ?, updated_at = ?
		 WHERE id = ? AND state = 'accepted'`,
		StateRunning, sqlite.FormatTime(updatedAt.UTC()), id)
}

func (s *SQLiteStore) MarkTerminal(ctx context.Context, id, state, resultEventID, errorCode string, updatedAt time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, Errorf(ErrorCodeInvalidArgument, "db must not be nil")
	}
	switch state {
	case StateCompleted, StateFailed, StateCancelled:
	default:
		return false, Errorf(ErrorCodeInvalidArgument, "terminal state must be completed, failed, or cancelled")
	}
	return s.advanceState(ctx,
		`UPDATE webhook_execution
		 SET state = ?, result_event_id = ?, error_code = ?, updated_at = ?
		 WHERE id = ? AND state IN ('accepted','running')`,
		state, nullText(resultEventID), nullText(errorCode), sqlite.FormatTime(updatedAt.UTC()), id)
}

func (s *SQLiteStore) advanceState(ctx context.Context, query string, args ...any) (bool, error) {
	result, err := sqlite.Conn(ctx, s.db).ExecContext(ctx, query, args...)
	if err != nil {
		return false, sqlite.Classify(fmt.Errorf("transition webhook execution: %w", err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, sqlite.Classify(fmt.Errorf("transition webhook execution count: %w", err))
	}
	return affected > 0, nil
}

func (s *SQLiteStore) DeleteExecution(ctx context.Context, id string) error {
	if s == nil || s.db == nil {
		return Errorf(ErrorCodeInvalidArgument, "db must not be nil")
	}
	if _, err := sqlite.Conn(ctx, s.db).ExecContext(ctx, `DELETE FROM webhook_execution WHERE id = ?`, id); err != nil {
		return sqlite.Classify(fmt.Errorf("delete webhook execution: %w", err))
	}
	return nil
}

func (s *SQLiteStore) ListUnfinished(ctx context.Context, query *UnfinishedQuery) ([]Execution, error) {
	if s == nil || s.db == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "db must not be nil")
	}
	if query == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "query must not be nil")
	}
	if query.Limit <= 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "unfinished execution limit must be positive")
	}
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx,
		`SELECT `+selectExecutionColumns+` FROM webhook_execution
		 WHERE state IN ('accepted','running') AND expires_at > ? AND id > ?
		 ORDER BY id LIMIT ?`,
		sqlite.FormatTime(query.Now.UTC()), query.AfterID, query.Limit)
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list unfinished webhook executions: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var executions []Execution
	for rows.Next() {
		execution, err := scanExecution(rows)
		if err != nil {
			return nil, err
		}
		executions = append(executions, execution)
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list unfinished webhook executions: %w", err))
	}
	return executions, nil
}

const selectExecutionColumns = `id, key_id, event_id, nonce, body_digest, turn_id, session_id, state,
	result_event_id, error_code, created_at, updated_at, expires_at`

func (s *SQLiteStore) findExecution(ctx context.Context, query string, args ...any) (Execution, bool, error) {
	execution, err := scanExecution(sqlite.Conn(ctx, s.db).QueryRowContext(ctx, query, args...))
	switch {
	case err == nil:
		return execution, true, nil
	case errors.Is(err, sql.ErrNoRows):
		return Execution{}, false, nil
	default:
		return Execution{}, false, err
	}
}

func scanExecution(row rowScanner) (Execution, error) {
	var execution Execution
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
		return Execution{}, err
	default:
		return Execution{}, sqlite.Classify(fmt.Errorf("read webhook execution: %w", err))
	}
	if resultEventID.Valid {
		execution.ResultEventID = resultEventID.String
	}
	if errorCode.Valid {
		execution.ErrorCode = errorCode.String
	}
	var parseErr error
	if execution.CreatedAt, parseErr = sqlite.ParseTime(createdAtRaw); parseErr != nil {
		return Execution{}, Errorf(ErrorCodeExecutionInvalid, "stored creation timestamp is not a valid time")
	}
	if execution.UpdatedAt, parseErr = sqlite.ParseTime(updatedAtRaw); parseErr != nil {
		return Execution{}, Errorf(ErrorCodeExecutionInvalid, "stored update timestamp is not a valid time")
	}
	if execution.ExpiresAt, parseErr = sqlite.ParseTime(expiresAtRaw); parseErr != nil {
		return Execution{}, Errorf(ErrorCodeExecutionInvalid, "stored expiry timestamp is not a valid time")
	}
	return execution, nil
}

func nullText(value string) sql.NullString {
	if value == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: value, Valid: true}
}
