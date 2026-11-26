package turnstore

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
	"github.com/anggasct/aura/internal/platform/turn"
)

type EventStore interface {
	Append(ctx context.Context, e *turn.Event) error
	AppendBatch(ctx context.Context, events []turn.Event) error
	LastSequence(ctx context.Context, sessionID string) (uint64, error)
	AppendSequenced(ctx context.Context, sessionID string, e *turn.Event) (uint64, error)
	UpsertEvent(ctx context.Context, e *turn.Event) (uint64, bool, error)
}

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

func (s *sqliteEventStore) Append(ctx context.Context, e *turn.Event) error {
	if e == nil {
		return errNilArgument("event")
	}
	return appendEvent(ctx, sqlite.Conn(ctx, s.db), e)
}

func (s *sqliteEventStore) AppendBatch(ctx context.Context, events []turn.Event) error {
	if len(events) == 0 {
		return nil
	}
	return sqlite.RunInTx(ctx, s.db, func(ctx context.Context) error {
		stmt, err := sqlite.Conn(ctx, s.db).PrepareContext(ctx, insertRuntimeEventSQL)
		if err != nil {
			return classify(fmt.Errorf("prepare append batch: %w", err))
		}
		defer func() { _ = stmt.Close() }()

		for i := range events {
			if err := appendEventCore(ctx, stmt.ExecContext, &events[i]); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *sqliteEventStore) LastSequence(ctx context.Context, sessionID string) (uint64, error) {
	var seq sql.NullInt64
	err := sqlite.Conn(ctx, s.db).QueryRowContext(ctx,
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

func (s *sqliteEventStore) AppendSequenced(ctx context.Context, sessionID string, e *turn.Event) (uint64, error) {
	if e == nil {
		return 0, errNilArgument("event")
	}
	var assigned uint64
	err := sqlite.RunInTx(ctx, s.db, func(ctx context.Context) error {
		q := sqlite.Conn(ctx, s.db)
		seq, err := assignNextSequence(ctx, q, sessionID)
		if err != nil {
			return err
		}
		e.Sequence = seq
		if err := appendEvent(ctx, q, e); err != nil {
			return err
		}
		assigned = seq
		return nil
	})
	if err != nil {
		return 0, err
	}
	return assigned, nil
}

func (s *sqliteEventStore) UpsertEvent(ctx context.Context, e *turn.Event) (sequence uint64, existed bool, err error) {
	if e == nil {
		return 0, false, errNilArgument("event")
	}
	if e.ID == "" {
		return 0, false, errorf(ErrorCodeInvalidArgument, "upsert event requires an id")
	}
	err = sqlite.RunInTx(ctx, s.db, func(ctx context.Context) error {
		q := sqlite.Conn(ctx, s.db)
		var stored int64
		lookupErr := q.QueryRowContext(ctx, `SELECT sequence FROM runtime_event WHERE id = ?`, e.ID).Scan(&stored)
		switch {
		case lookupErr == nil:
			assigned, seqErr := sequenceFromDB(stored)
			if seqErr != nil {
				return seqErr
			}
			sequence = assigned
			existed = true
			return nil
		case errors.Is(lookupErr, sql.ErrNoRows):
		default:
			return classify(fmt.Errorf("lookup upsert event: %w", lookupErr))
		}
		assigned, seqErr := assignNextSequence(ctx, q, e.SessionID)
		if seqErr != nil {
			return seqErr
		}
		e.Sequence = assigned
		if appendErr := appendEvent(ctx, q, e); appendErr != nil {
			return appendErr
		}
		sequence = assigned
		existed = false
		return nil
	})
	if err != nil {
		return 0, false, err
	}
	return sequence, existed, nil
}

func assignNextSequence(ctx context.Context, q sqlite.Querier, sessionID string) (uint64, error) {
	var highest sql.NullInt64
	if err := q.QueryRowContext(ctx,
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

func (s *sqliteEventStore) LookupEvent(ctx context.Context, id string) (turn.Event, bool, error) {
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx,
		`SELECT `+selectRuntimeEventColumns+` FROM runtime_event WHERE id = ?`, id,
	)
	if err != nil {
		return turn.Event{}, false, classify(fmt.Errorf("lookup event %s: %w", id, err))
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return turn.Event{}, false, classify(fmt.Errorf("lookup event %s: %w", id, err))
		}
		return turn.Event{}, false, nil
	}
	event, err := scanRuntimeEvent(rows)
	if err != nil {
		return turn.Event{}, false, err
	}
	return event, true, nil
}

func (s *sqliteEventStore) AppendCheckpoint(ctx context.Context, event *turn.Event) error {
	if event == nil {
		return errNilArgument("event")
	}
	return sqlite.RunInTx(ctx, s.db, func(ctx context.Context) error {
		q := sqlite.Conn(ctx, s.db)
		var sessionID, turnID, invocationID, branch, author, kind, payload string
		var schemaVersion int64
		err := q.QueryRowContext(ctx, `
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
		return appendEvent(ctx, q, event)
	})
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

func appendEvent(ctx context.Context, q sqlite.Querier, e *turn.Event) error {
	return appendEventCore(ctx, func(ctx context.Context, args ...any) (sql.Result, error) {
		return q.ExecContext(ctx, insertRuntimeEventSQL, args...)
	}, e)
}

func appendEventCore(ctx context.Context, exec func(context.Context, ...any) (sql.Result, error), e *turn.Event) error {
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

func nullableJSON(raw json.RawMessage) any {
	if raw == nil {
		return nil
	}
	return string(raw)
}

const selectRuntimeEventColumns = `id, session_id, sequence, turn_id, invocation_id, branch, author, kind,
	                                schema_version, payload_json, provider_usage_json, created_at`

func scanRuntimeEvent(rows *sql.Rows) (turn.Event, error) {
	var e turn.Event
	var sequence, schemaVersion int64
	var payload, createdAt string
	var providerUsage sql.NullString
	if err := rows.Scan(&e.ID, &e.SessionID, &sequence, &e.TurnID, &e.InvocationID, &e.Branch, &e.Author, &e.Kind,
		&schemaVersion, &payload, &providerUsage, &createdAt); err != nil {
		return turn.Event{}, classify(fmt.Errorf("scan runtime event: %w", err))
	}
	var err error
	if e.Sequence, err = sequenceFromDB(sequence); err != nil {
		return turn.Event{}, err
	}
	if e.SchemaVersion, err = schemaVersionFromDB(schemaVersion); err != nil {
		return turn.Event{}, err
	}
	e.Payload = json.RawMessage(payload)
	if providerUsage.Valid {
		e.ProviderUsage = json.RawMessage(providerUsage.String)
	}
	if e.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return turn.Event{}, fmt.Errorf("parse created_at for event %s: %w", e.ID, err)
	}
	return e, nil
}
