package model

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

type sqliteCircuitCheckpointStore struct {
	db *sql.DB
}

func NewSQLiteCircuitCheckpointStore(db *sql.DB) CircuitCheckpointStore {
	return &sqliteCircuitCheckpointStore{db: db}
}

func (s *sqliteCircuitCheckpointStore) Save(ctx context.Context, cp *CircuitCheckpoint) error {
	if s.db == nil || cp == nil {
		return nil
	}
	if cp.CircuitKey == "" {
		return fault.New(ErrorCodeInvalidArgument, "circuit_key must not be empty")
	}

	var openUntilRaw *string
	if cp.OpenUntil != nil && !cp.OpenUntil.IsZero() {
		formatted := sqlite.FormatTime(cp.OpenUntil.UTC())
		openUntilRaw = &formatted
	}
	updatedAtRaw := sqlite.FormatTime(cp.UpdatedAt.UTC())

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
	_, err := sqlite.Conn(ctx, s.db).ExecContext(ctx, query,
		cp.CircuitKey,
		cp.ConfigDigest,
		string(cp.State),
		cp.ConsecutiveFailures,
		openUntilRaw,
		updatedAtRaw,
	)
	if err != nil {
		return sqlite.Classify(fmt.Errorf("save circuit checkpoint: %w", err))
	}
	return nil
}

func (s *sqliteCircuitCheckpointStore) Load(ctx context.Context) ([]CircuitCheckpoint, error) {
	if s.db == nil {
		return nil, nil
	}
	query := `
SELECT circuit_key, config_digest, state, consecutive_failures, open_until, updated_at
FROM model_circuit_checkpoint
ORDER BY circuit_key ASC
`
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx, query)
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("load circuit checkpoints: %w", err))
	}
	defer func() { _ = rows.Close() }()

	var results []CircuitCheckpoint
	for rows.Next() {
		var cp CircuitCheckpoint
		var stateRaw string
		var openUntilRaw sql.NullString
		var updatedAtRaw string

		if err := rows.Scan(
			&cp.CircuitKey,
			&cp.ConfigDigest,
			&stateRaw,
			&cp.ConsecutiveFailures,
			&openUntilRaw,
			&updatedAtRaw,
		); err != nil {
			return nil, sqlite.Classify(fmt.Errorf("scan circuit checkpoint: %w", err))
		}
		cp.State = CircuitState(stateRaw)

		if openUntilRaw.Valid && openUntilRaw.String != "" {
			t, err := sqlite.ParseTime(openUntilRaw.String)
			if err == nil {
				cp.OpenUntil = &t
			}
		}
		if t, err := sqlite.ParseTime(updatedAtRaw); err == nil {
			cp.UpdatedAt = t
		}
		results = append(results, cp)
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("iterate circuit checkpoints: %w", err))
	}
	return results, nil
}

func (s *sqliteCircuitCheckpointStore) Delete(ctx context.Context, circuitKey string) error {
	if s.db == nil {
		return nil
	}
	query := `DELETE FROM model_circuit_checkpoint WHERE circuit_key = ?`
	_, err := sqlite.Conn(ctx, s.db).ExecContext(ctx, query, circuitKey)
	if err != nil {
		return sqlite.Classify(fmt.Errorf("delete circuit checkpoint: %w", err))
	}
	return nil
}
