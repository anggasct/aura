package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
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
		return classify(fmt.Errorf("begin append batch: %w", err))
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, insertRuntimeEventSQL)
	if err != nil {
		return classify(fmt.Errorf("prepare append batch: %w", err))
	}
	defer func() { _ = stmt.Close() }()

	for i := range events {
		if err := appendEventCore(ctx, stmt.ExecContext, &events[i]); err != nil {
			return err
		}
	}
	return classify(tx.Commit())
}

func (s *sqliteEventStore) LastSequence(ctx context.Context, sessionID string) (uint64, error) {
	var seq sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(sequence) FROM runtime_event WHERE session_id = ?`, sessionID,
	).Scan(&seq)
	if err != nil {
		return 0, classify(fmt.Errorf("last sequence for session %s: %w", sessionID, err))
	}
	if !seq.Valid {
		return 0, nil
	}
	return sequenceFromDB(seq.Int64)
}

func (s *sqliteEventStore) AppendSequenced(ctx context.Context, sessionID string, e *RuntimeEvent) (uint64, error) {
	if e == nil {
		return 0, errNilArgument("event")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, classify(fmt.Errorf("begin sequenced append: %w", err))
	}
	defer func() { _ = tx.Rollback() }()
	assigned, err := assignNextSequence(ctx, tx, sessionID)
	if err != nil {
		return 0, err
	}
	e.Sequence = assigned
	if err := appendEvent(ctx, tx, e); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, classify(fmt.Errorf("commit sequenced append: %w", err))
	}
	return assigned, nil
}

func (s *sqliteEventStore) UpsertEvent(ctx context.Context, e *RuntimeEvent) (sequence uint64, existed bool, err error) {
	if e == nil {
		return 0, false, errNilArgument("event")
	}
	if e.ID == "" {
		return 0, false, Errorf(ErrorCodeInvalidArgument, "upsert event requires an id")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, classify(fmt.Errorf("begin upsert event: %w", err))
	}
	defer func() { _ = tx.Rollback() }()
	var stored int64
	err = tx.QueryRowContext(ctx, `SELECT sequence FROM runtime_event WHERE id = ?`, e.ID).Scan(&stored)
	switch {
	case err == nil:
		assigned, err := sequenceFromDB(stored)
		if err != nil {
			return 0, false, err
		}
		if err := tx.Commit(); err != nil {
			return 0, false, classify(fmt.Errorf("commit upsert event: %w", err))
		}
		return assigned, true, nil
	case errors.Is(err, sql.ErrNoRows):
	default:
		return 0, false, classify(fmt.Errorf("lookup upsert event: %w", err))
	}
	assigned, err := assignNextSequence(ctx, tx, e.SessionID)
	if err != nil {
		return 0, false, err
	}
	e.Sequence = assigned
	if err := appendEvent(ctx, tx, e); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, classify(fmt.Errorf("commit upsert event: %w", err))
	}
	return assigned, false, nil
}

func assignNextSequence(ctx context.Context, tx *sql.Tx, sessionID string) (uint64, error) {
	var highest sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT MAX(sequence) FROM runtime_event WHERE session_id = ?`, sessionID,
	).Scan(&highest); err != nil {
		return 0, classify(fmt.Errorf("read max sequence for session %s: %w", sessionID, err))
	}
	next := int64(1)
	if highest.Valid {
		if highest.Int64 == math.MaxInt64 {
			return 0, fault.New(
				ErrorCodeEventSequenceInvalid,
				fmt.Sprintf("stored sequence for session %s is exhausted", sessionID),
			)
		}
		next = highest.Int64 + 1
	}
	return sequenceFromDB(next)
}

func AllocateSequenceTx(ctx context.Context, tx *sql.Tx, sessionID string) (uint64, error) {
	if tx == nil {
		return 0, errNilArgument("tx")
	}
	if sessionID == "" {
		return 0, fault.New(ErrorCodeInvalidArgument, "session id must not be empty")
	}
	if !rowExists(ctx, tx, `SELECT 1 FROM session WHERE id = ?`, sessionID) {
		return 0, fault.New(ErrorCodeSessionNotFound, "session does not exist")
	}
	sequence, err := assignNextSequence(ctx, tx, sessionID)
	if err != nil {
		return 0, classify(fmt.Errorf("allocate sequence: %w", err))
	}
	return sequence, nil
}

func (s *sqliteEventStore) LookupEvent(ctx context.Context, id string) (RuntimeEvent, bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+selectRuntimeEventColumns+` FROM runtime_event WHERE id = ?`, id,
	)
	if err != nil {
		return RuntimeEvent{}, false, classify(fmt.Errorf("lookup event %s: %w", id, err))
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return RuntimeEvent{}, false, classify(fmt.Errorf("lookup event %s: %w", id, err))
		}
		return RuntimeEvent{}, false, nil
	}
	event, err := scanRuntimeEvent(rows)
	if err != nil {
		return RuntimeEvent{}, false, err
	}
	return event, true, nil
}

func (s *sqliteEventStore) AppendCheckpoint(ctx context.Context, event *RuntimeEvent) error {
	if event == nil {
		return errNilArgument("event")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return classify(fmt.Errorf("begin checkpoint append: %w", err))
	}
	defer func() { _ = tx.Rollback() }()

	var sessionID, turnID, invocationID, branch, author, kind, payload string
	var schemaVersion int64
	err = tx.QueryRowContext(ctx, `
		SELECT session_id, turn_id, invocation_id, branch, author, kind, schema_version, payload_json
		FROM runtime_event WHERE id = ?`, event.ID,
	).Scan(&sessionID, &turnID, &invocationID, &branch, &author, &kind, &schemaVersion, &payload)
	if err == nil {
		existingSchemaVersion, schemaErr := schemaVersionFromDB(schemaVersion)
		if schemaErr != nil {
			return schemaErr
		}
		if sessionID == event.SessionID && turnID == event.TurnID && invocationID == event.InvocationID &&
			branch == event.Branch && author == event.Author && kind == event.Kind &&
			existingSchemaVersion == event.SchemaVersion && payload == string(event.Payload) {
			return nil
		}
		return fault.New(
			ErrorCodeEventSequenceConflict,
			fmt.Sprintf("event %s is already bound to different checkpoint state", event.ID),
		)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return classify(fmt.Errorf("lookup checkpoint event %s: %w", event.ID, err))
	}
	if err := AppendEventTx(ctx, tx, event); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return classify(fmt.Errorf("commit checkpoint append: %w", err))
	}
	return nil
}

func sequenceToDB(sequence uint64) (int64, error) {
	if sequence > math.MaxInt64 {
		return 0, fault.New(
			ErrorCodeEventSequenceInvalid,
			fmt.Sprintf("sequence %d exceeds the maximum storable value", sequence),
		)
	}
	return int64(sequence), nil
}

func sequenceFromDB(stored int64) (uint64, error) {
	if stored < 0 {
		return 0, fault.New(
			ErrorCodeEventSequenceInvalid,
			fmt.Sprintf("stored sequence %d is negative", stored),
		)
	}
	return uint64(stored), nil
}

func schemaVersionFromDB(stored int64) (uint16, error) {
	if stored < 0 || stored > math.MaxUint16 {
		return 0, fault.New(
			ErrorCodeEventSchemaVersionInvalid,
			fmt.Sprintf("stored schema version %d is out of range", stored),
		)
	}
	return uint16(stored), nil
}

func schemaVersionToDB(version uint16) (int64, error) {
	if version == 0 {
		return 0, fault.New(
			ErrorCodeEventSchemaVersionInvalid,
			"schema version must be positive",
		)
	}
	return int64(version), nil
}

type EventWriter interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func AppendEventTx(ctx context.Context, tx EventWriter, e *RuntimeEvent) error {
	if tx == nil {
		return errNilArgument("tx")
	}
	if e == nil {
		return errNilArgument("event")
	}
	return appendEvent(ctx, tx, e)
}

func appendEvent(ctx context.Context, x EventWriter, e *RuntimeEvent) error {
	return appendEventCore(ctx, func(ctx context.Context, args ...any) (sql.Result, error) {
		return x.ExecContext(ctx, insertRuntimeEventSQL, args...)
	}, e)
}

func appendEventCore(ctx context.Context, exec func(context.Context, ...any) (sql.Result, error), e *RuntimeEvent) error {
	if !json.Valid(e.Payload) {
		return fault.New(
			ErrorCodeEventPayloadInvalid,
			fmt.Sprintf("event %s payload is not valid JSON", e.ID),
		)
	}
	if e.Sequence == 0 {
		return fault.New(
			ErrorCodeEventSequenceInvalid,
			fmt.Sprintf("event %s sequence must be positive", e.ID),
		)
	}
	if e.SchemaVersion == 0 {
		return fault.New(
			ErrorCodeEventSchemaVersionInvalid,
			fmt.Sprintf("event %s schema version must be positive", e.ID),
		)
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
		schemaVersion, string(e.Payload), nullableJSON(e.ProviderUsage), sqlite.FormatTime(e.CreatedAt),
	)
	if err != nil {
		if isConstraintUnique(err) {
			return fault.New(
				ErrorCodeEventSequenceConflict,
				fmt.Sprintf("session %s sequence %d already used by a different event", e.SessionID, e.Sequence),
			)
		}
		if isConstraintForeignKey(err) {
			return fault.New(
				ErrorCodeSessionNotFound,
				fmt.Sprintf("session %s does not exist", e.SessionID),
			)
		}
		if isConstraintCheck(err) {
			return fault.New(
				ErrorCodeEventPayloadInvalid,
				fmt.Sprintf("event %s violates a storage constraint", e.ID),
			)
		}
		return classify(fmt.Errorf("append event %s: %w", e.ID, err))
	}
	return nil
}

func isConstraintUnique(err error) bool {
	classified := sqlite.Classify(err)
	return fault.HasCode(classified, sqlite.ErrorCodeConstraintUnique) ||
		fault.HasCode(classified, sqlite.ErrorCodeConstraintPrimaryKey)
}

func isConstraintForeignKey(err error) bool {
	return fault.HasCode(sqlite.Classify(err), sqlite.ErrorCodeConstraintForeignKey)
}

func isConstraintCheck(err error) bool {
	return fault.HasCode(sqlite.Classify(err), sqlite.ErrorCodeConstraintCheck)
}

func nullableJSON(raw json.RawMessage) any {
	if raw == nil {
		return nil
	}
	return string(raw)
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
		return RuntimeEvent{}, classify(fmt.Errorf("scan runtime event: %w", err))
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
