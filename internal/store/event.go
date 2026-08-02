package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"modernc.org/sqlite"
)

// SQLite extended result codes, see https://www.sqlite.org/rescode.html.
const (
	sqliteConstraintUnique     = 2067 // SQLITE_CONSTRAINT_UNIQUE
	sqliteConstraintPrimaryKey = 1555 // SQLITE_CONSTRAINT_PRIMARYKEY
	sqliteConstraintCheck      = 275  // SQLITE_CONSTRAINT_CHECK
	sqliteConstraintForeignKey = 787  // SQLITE_CONSTRAINT_FOREIGNKEY
)

const insertRuntimeEventSQL = `
INSERT INTO runtime_event (
    id, session_id, sequence, turn_id, invocation_id, branch, author, kind,
    schema_version, payload_json, provider_usage_json, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO NOTHING`

type sqliteEventStore struct {
	db *sql.DB
}

func NewEventStore(db *sql.DB) EventStore {
	return &sqliteEventStore{db: db}
}

func (s *sqliteEventStore) Append(ctx context.Context, e *RuntimeEvent) error {
	if e == nil {
		return errNilArgument("event")
	}
	return appendEvent(ctx, s.db, e)
}

func (s *sqliteEventStore) AppendBatch(ctx context.Context, events []RuntimeEvent) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin append batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, insertRuntimeEventSQL)
	if err != nil {
		return fmt.Errorf("prepare append batch: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for i := range events {
		if err := appendEventCore(ctx, stmt.ExecContext, &events[i]); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *sqliteEventStore) LastSequence(ctx context.Context, sessionID string) (uint64, error) {
	var seq sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(sequence) FROM runtime_event WHERE session_id = ?`, sessionID,
	).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("last sequence for session %s: %w", sessionID, err)
	}
	if !seq.Valid {
		return 0, nil
	}
	return sequenceFromDB(seq.Int64)
}

// sequenceToDB and sequenceFromDB bridge the uint64 domain counter and the
// signed INTEGER column. Out-of-range values are rejected rather than wrapped,
// so a corrupt row can never present itself as a valid ordering position.
// Zero is valid here: it is the "from the beginning" cursor in ListEvents and
// only the append boundary rejects it.
func sequenceToDB(sequence uint64) (int64, error) {
	if sequence > math.MaxInt64 {
		return 0, &Error{
			Code:   ErrorCodeEventSequenceInvalid,
			Detail: fmt.Sprintf("sequence %d exceeds the maximum storable value", sequence),
		}
	}
	return int64(sequence), nil
}

func sequenceFromDB(stored int64) (uint64, error) {
	if stored < 0 {
		return 0, &Error{
			Code:   ErrorCodeEventSequenceInvalid,
			Detail: fmt.Sprintf("stored sequence %d is negative", stored),
		}
	}
	return uint64(stored), nil
}

func schemaVersionFromDB(stored int64) (uint16, error) {
	if stored < 0 || stored > math.MaxUint16 {
		return 0, &Error{
			Code:   ErrorCodeEventSchemaVersionInvalid,
			Detail: fmt.Sprintf("stored schema version %d is out of range", stored),
		}
	}
	return uint16(stored), nil
}

// schemaVersionToDB rejects a zero schema version at the boundary, mirroring
// the schema's CHECK (schema_version > 0).
func schemaVersionToDB(version uint16) (int64, error) {
	if version == 0 {
		return 0, &Error{
			Code:   ErrorCodeEventSchemaVersionInvalid,
			Detail: "schema version must be positive",
		}
	}
	return int64(version), nil
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func appendEvent(ctx context.Context, x execer, e *RuntimeEvent) error {
	return appendEventCore(ctx, func(ctx context.Context, args ...any) (sql.Result, error) {
		return x.ExecContext(ctx, insertRuntimeEventSQL, args...)
	}, e)
}

func appendEventCore(ctx context.Context, exec func(context.Context, ...any) (sql.Result, error), e *RuntimeEvent) error {
	if !json.Valid(e.Payload) {
		return &Error{
			Code:   ErrorCodeEventPayloadInvalid,
			Detail: fmt.Sprintf("event %s payload is not valid JSON", e.ID),
		}
	}
	if e.Sequence == 0 {
		return &Error{
			Code:   ErrorCodeEventSequenceInvalid,
			Detail: fmt.Sprintf("event %s sequence must be positive", e.ID),
		}
	}
	if e.SchemaVersion == 0 {
		return &Error{
			Code:   ErrorCodeEventSchemaVersionInvalid,
			Detail: fmt.Sprintf("event %s schema version must be positive", e.ID),
		}
	}
	sequence, err := sequenceToDB(e.Sequence)
	if err != nil {
		return err
	}
	schemaVersion, err := schemaVersionToDB(e.SchemaVersion)
	if err != nil {
		return err
	}
	_, err = exec(ctx,
		e.ID, e.SessionID, sequence, e.TurnID, e.InvocationID, e.Branch, e.Author, e.Kind,
		schemaVersion, string(e.Payload), nullableJSON(e.ProviderUsage), formatTime(e.CreatedAt),
	)
	if err != nil {
		if isConstraintUnique(err) {
			return &Error{
				Code:   ErrorCodeEventSequenceConflict,
				Detail: fmt.Sprintf("session %s sequence %d already used by a different event", e.SessionID, e.Sequence),
			}
		}
		if isConstraintForeignKey(err) {
			return &Error{
				Code:   ErrorCodeSessionNotFound,
				Detail: fmt.Sprintf("session %s does not exist", e.SessionID),
			}
		}
		if isConstraintCheck(err) {
			return &Error{
				Code:   ErrorCodeEventPayloadInvalid,
				Detail: fmt.Sprintf("event %s violates a storage constraint", e.ID),
			}
		}
		return classifyBusy(fmt.Errorf("append event %s: %w", e.ID, err))
	}
	return nil
}

func isConstraintUnique(err error) bool {
	return isConstraintCode(err, sqliteConstraintUnique, sqliteConstraintPrimaryKey)
}

func isConstraintForeignKey(err error) bool {
	return isConstraintCode(err, sqliteConstraintForeignKey)
}

func isConstraintCheck(err error) bool {
	return isConstraintCode(err, sqliteConstraintCheck)
}

func isConstraintCode(err error, codes ...int) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	for _, code := range codes {
		if sqliteErr.Code() == code {
			return true
		}
	}
	return false
}

func nullableJSON(raw json.RawMessage) any {
	if raw == nil {
		return nil
	}
	return string(raw)
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

const selectRuntimeEventColumns = `id, session_id, sequence, turn_id, invocation_id, branch, author, kind,
	                                schema_version, payload_json, provider_usage_json, created_at`

func scanRuntimeEvent(rows *sql.Rows) (RuntimeEvent, error) {
	var e RuntimeEvent
	var sequence, schemaVersion int64
	var payload, createdAt string
	var providerUsage sql.NullString
	if err := rows.Scan(&e.ID, &e.SessionID, &sequence, &e.TurnID, &e.InvocationID, &e.Branch, &e.Author, &e.Kind,
		&schemaVersion, &payload, &providerUsage, &createdAt); err != nil {
		return RuntimeEvent{}, fmt.Errorf("scan runtime event: %w", err)
	}
	var err error
	if e.Sequence, err = sequenceFromDB(sequence); err != nil {
		return RuntimeEvent{}, err
	}
	if e.SchemaVersion, err = schemaVersionFromDB(schemaVersion); err != nil {
		return RuntimeEvent{}, err
	}
	e.Payload = json.RawMessage(payload)
	if providerUsage.Valid {
		e.ProviderUsage = json.RawMessage(providerUsage.String)
	}
	if e.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return RuntimeEvent{}, fmt.Errorf("parse created_at for event %s: %w", e.ID, err)
	}
	return e, nil
}
