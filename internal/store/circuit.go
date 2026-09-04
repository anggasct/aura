package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

func parseTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, s)
}

type CircuitCheckpoint struct {
	CircuitKey          string
	ConfigDigest        string
	State               string
	ConsecutiveFailures int
	OpenUntil           *time.Time
	UpdatedAt           time.Time
}

type CircuitCheckpointStore interface {
	Save(ctx context.Context, cp *CircuitCheckpoint) error
	Load(ctx context.Context) ([]CircuitCheckpoint, error)
	Delete(ctx context.Context, circuitKey string) error
}

type sqliteCircuitStore struct {
	db *sql.DB
}

func NewCircuitCheckpointStore(db *sql.DB) CircuitCheckpointStore {
	return &sqliteCircuitStore{db: db}
}

func (s *sqliteCircuitStore) Save(ctx context.Context, cp *CircuitCheckpoint) error {
	if s.db == nil || cp == nil {
		return nil
	}
	if cp.CircuitKey == "" {
		return Errorf(ErrorCodeInvalidArgument, "circuit_key must not be empty")
	}

	var openUntilRaw *string
	if cp.OpenUntil != nil && !cp.OpenUntil.IsZero() {
		formatted := formatTime(cp.OpenUntil.UTC())
		openUntilRaw = &formatted
	}
	updatedAtRaw := formatTime(cp.UpdatedAt.UTC())

	query := `
INSERT INTO model_circuit_checkpoint (
    circuit_key, config_digest, state, consecutive_failures, open_until, updated_at
) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(circuit_key) DO UPDATE SET
    config_digest = excluded.config_digest,
    state = excluded.state,
    consecutive_failures = excluded.consecutive_failures,
    open_until = excluded.open_until,
    updated_at = excluded.updated_at
`
	_, err := s.db.ExecContext(ctx, query,
		cp.CircuitKey,
		cp.ConfigDigest,
		cp.State,
		cp.ConsecutiveFailures,
		openUntilRaw,
		updatedAtRaw,
	)
	if err != nil {
		return classifyBusy(fmt.Errorf("save circuit checkpoint: %w", err))
	}
	return nil
}

func (s *sqliteCircuitStore) Load(ctx context.Context) ([]CircuitCheckpoint, error) {
	if s.db == nil {
		return nil, nil
	}
	query := `
SELECT circuit_key, config_digest, state, consecutive_failures, open_until, updated_at
FROM model_circuit_checkpoint
ORDER BY circuit_key ASC
`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("load circuit checkpoints: %w", err))
	}
	defer func() { _ = rows.Close() }()

	var results []CircuitCheckpoint
	for rows.Next() {
		var cp CircuitCheckpoint
		var openUntilRaw sql.NullString
		var updatedAtRaw string

		if err := rows.Scan(
			&cp.CircuitKey,
			&cp.ConfigDigest,
			&cp.State,
			&cp.ConsecutiveFailures,
			&openUntilRaw,
			&updatedAtRaw,
		); err != nil {
			return nil, classifyBusy(fmt.Errorf("scan circuit checkpoint: %w", err))
		}

		if openUntilRaw.Valid && openUntilRaw.String != "" {
			t, err := parseTime(openUntilRaw.String)
			if err == nil {
				cp.OpenUntil = &t
			}
		}
		if t, err := parseTime(updatedAtRaw); err == nil {
			cp.UpdatedAt = t
		}
		results = append(results, cp)
	}
	if err := rows.Err(); err != nil {
		return nil, classifyBusy(fmt.Errorf("iterate circuit checkpoints: %w", err))
	}
	return results, nil
}

func (s *sqliteCircuitStore) Delete(ctx context.Context, circuitKey string) error {
	if s.db == nil {
		return nil
	}
	query := `DELETE FROM model_circuit_checkpoint WHERE circuit_key = ?`
	_, err := s.db.ExecContext(ctx, query, circuitKey)
	if err != nil {
		return classifyBusy(fmt.Errorf("delete circuit checkpoint: %w", err))
	}
	return nil
}
