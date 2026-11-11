package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type DedupeStore interface {
	Accept(ctx context.Context, source, externalID string, expiresAt time.Time, accepted *RuntimeEvent) (originalTurnID string, created bool, err error)
	Claimed(ctx context.Context, source, externalID string) (bool, error)
	ListTurnEvents(ctx context.Context, turnID string) ([]RuntimeEvent, error)
}

type DedupeClaim struct {
	Source     string
	ExternalID string
	TurnID     string
	Now        time.Time
	ExpiresAt  time.Time
}

type sqliteDedupeStore struct {
	db *sql.DB
}

func NewDedupeStore(db *sql.DB) DedupeStore {
	return &sqliteDedupeStore{db: db}
}

func ClaimDedupeTx(ctx context.Context, tx *sql.Tx, claim *DedupeClaim) (originalTurnID string, created bool, err error) {
	if tx == nil {
		return "", false, errNilArgument("tx")
	}
	if claim == nil {
		return "", false, errNilArgument("claim")
	}
	if claim.Source == "" || claim.ExternalID == "" {
		return "", false, Errorf(ErrorCodeInvalidArgument, "dedupe key must have a source and an external id")
	}
	if claim.Now.IsZero() {
		return "", false, Errorf(ErrorCodeInvalidArgument, "dedupe claim must have a current time")
	}
	var claimedTurnID string
	var expiresAtRaw string
	err = tx.QueryRowContext(ctx,
		`SELECT turn_id, expires_at FROM ingress_dedupe WHERE source = ? AND external_id = ?`,
		claim.Source, claim.ExternalID,
	).Scan(&claimedTurnID, &expiresAtRaw)
	switch {
	case err == nil:
		storedExpiry, parseErr := parseTime(expiresAtRaw)
		if parseErr != nil {
			return "", false, fmt.Errorf("parse dedupe expiry: %w", parseErr)
		}
		if !storedExpiry.Before(claim.Now) {
			return claimedTurnID, false, nil
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM ingress_dedupe WHERE source = ? AND external_id = ?`, claim.Source, claim.ExternalID,
		); err != nil {
			return "", false, classifyBusy(fmt.Errorf("expire dedupe key: %w", err))
		}
	case errors.Is(err, sql.ErrNoRows):
	default:
		return "", false, fmt.Errorf("read dedupe key: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO ingress_dedupe (source, external_id, accepted_at, expires_at, turn_id) VALUES (?, ?, ?, ?, ?)`,
		claim.Source, claim.ExternalID, formatTime(claim.Now), formatTime(claim.ExpiresAt), claim.TurnID,
	); err != nil {
		return "", false, classifyBusy(fmt.Errorf("claim dedupe key: %w", err))
	}
	return claim.TurnID, true, nil
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

	originalTurnID, created, err = ClaimDedupeTx(ctx, tx, &DedupeClaim{
		Source:     source,
		ExternalID: externalID,
		TurnID:     accepted.TurnID,
		Now:        time.Now().UTC(),
		ExpiresAt:  expiresAt,
	})
	if err != nil || !created {
		return originalTurnID, false, err
	}

	if accepted.Sequence == 0 {
		assigned, err := assignNextSequence(ctx, tx, accepted.SessionID)
		if err != nil {
			return "", false, err
		}
		accepted.Sequence = assigned
	}
	if err := appendEvent(ctx, tx, accepted); err != nil {
		return "", false, err
	}
	if err := tx.Commit(); err != nil {
		return "", false, classifyBusy(fmt.Errorf("commit dedupe accept: %w", err))
	}
	return accepted.TurnID, true, nil
}

func (s *sqliteDedupeStore) Claimed(ctx context.Context, source, externalID string) (bool, error) {
	if source == "" || externalID == "" {
		return false, Errorf(ErrorCodeInvalidArgument, "dedupe key must have a source and an external id")
	}
	var expiresAtRaw string
	err := s.db.QueryRowContext(ctx,
		`SELECT expires_at FROM ingress_dedupe WHERE source = ? AND external_id = ?`,
		source, externalID,
	).Scan(&expiresAtRaw)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read dedupe key: %w", err)
	}
	return expiresAtRaw >= formatTime(time.Now().UTC()), nil
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
