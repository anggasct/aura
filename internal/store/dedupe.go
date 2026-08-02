package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// DedupeStore atomically claims ingress keys and reads back a turn's stored
// events. The claim and the turn.accepted event land in one transaction, so a
// crash cannot leave a claim pointing at an event that was never written.
type DedupeStore interface {
	// Accept claims (source, external_id) for the accepted event's turn.
	// When the key is already claimed and unexpired it writes nothing and
	// returns the original turn ID with created=false.
	Accept(ctx context.Context, source, externalID string, expiresAt time.Time, accepted *RuntimeEvent) (originalTurnID string, created bool, err error)
	// ListTurnEvents returns every stored event of a turn in sequence order.
	ListTurnEvents(ctx context.Context, turnID string) ([]RuntimeEvent, error)
}

type sqliteDedupeStore struct {
	db *sql.DB
}

// NewDedupeStore returns a DedupeStore backed by the ingress_dedupe table.
func NewDedupeStore(db *sql.DB) DedupeStore {
	return &sqliteDedupeStore{db: db}
}

func (s *sqliteDedupeStore) Accept(ctx context.Context, source, externalID string, expiresAt time.Time, accepted *RuntimeEvent) (originalTurnID string, created bool, err error) {
	if accepted == nil {
		return "", false, errNilArgument("accepted")
	}
	if source == "" || externalID == "" {
		return "", false, Errorf(ErrorCodeInvalidArgument, "dedupe key must have a source and an external id")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, fmt.Errorf("begin dedupe accept: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var claimedTurnID string
	var expiresAtRaw string
	err = tx.QueryRowContext(ctx,
		`SELECT turn_id, expires_at FROM ingress_dedupe WHERE source = ? AND external_id = ?`,
		source, externalID,
	).Scan(&claimedTurnID, &expiresAtRaw)
	switch {
	case err == nil:
		if expiresAtRaw >= formatTime(time.Now().UTC()) {
			return claimedTurnID, false, nil
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM ingress_dedupe WHERE source = ? AND external_id = ?`, source, externalID,
		); err != nil {
			return "", false, classifyBusy(fmt.Errorf("expire dedupe key: %w", err))
		}
	case errors.Is(err, sql.ErrNoRows):
		// Fresh key; fall through to the insert.
	default:
		return "", false, fmt.Errorf("read dedupe key: %w", err)
	}

	if err := appendEvent(ctx, tx, accepted); err != nil {
		return "", false, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO ingress_dedupe (source, external_id, accepted_at, expires_at, turn_id) VALUES (?, ?, ?, ?, ?)`,
		source, externalID, formatTime(time.Now().UTC()), formatTime(expiresAt), accepted.TurnID,
	); err != nil {
		return "", false, classifyBusy(fmt.Errorf("claim dedupe key: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return "", false, classifyBusy(fmt.Errorf("commit dedupe accept: %w", err))
	}
	return accepted.TurnID, true, nil
}

func (s *sqliteDedupeStore) ListTurnEvents(ctx context.Context, turnID string) ([]RuntimeEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+selectRuntimeEventColumns+`
		 FROM runtime_event
		 WHERE turn_id = ?
		 ORDER BY sequence ASC`,
		turnID,
	)
	if err != nil {
		return nil, fmt.Errorf("list turn %s events: %w", turnID, err)
	}
	defer func() { _ = rows.Close() }()

	var events []RuntimeEvent
	for rows.Next() {
		e, err := scanRuntimeEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("list turn %s events: %w", turnID, err)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list turn %s events: %w", turnID, err)
	}
	return events, nil
}
