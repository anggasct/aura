package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/anggasct/aura/internal/platform/sqlite"
)

type Correlation struct {
	Source     string
	EventType  string
	ExternalID string
	RunID      string
	SignalName string
	DedupeKey  string
	CreatedAt  time.Time
}

func (s *Store) BindCorrelation(ctx context.Context, correlation *Correlation) error {
	if correlation == nil {
		return codedError(ErrorCodeExecutorInvalid, "correlation binding must not be nil")
	}
	if correlation.Source == "" || correlation.EventType == "" || correlation.ExternalID == "" {
		return codedError(ErrorCodeExecutorInvalid, "correlation binding requires source, event type, and external id")
	}
	if correlation.RunID == "" || correlation.SignalName == "" {
		return codedError(ErrorCodeExecutorInvalid, "correlation binding requires run and signal")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := sqlite.Conn(ctx, s.db).ExecContext(ctx,
		`INSERT INTO workflow_correlation (source, event_type, external_id, run_id, signal_name, dedupe_key, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		correlation.Source, correlation.EventType, correlation.ExternalID,
		correlation.RunID, correlation.SignalName, correlation.DedupeKey, now,
	)
	if err != nil {
		if isConstraintUnique(err) {
			return codedError(ErrorCodeCorrelationConflict, "correlation binding already exists for this provider event")
		}
		return sqlite.Classify(fmt.Errorf("persist correlation binding: %w", err))
	}
	return nil
}

func (s *Store) ResolveCorrelation(ctx context.Context, source, eventType, externalID, dedupeKey string) (Correlation, error) {
	var correlation Correlation
	var createdAt string
	err := sqlite.Conn(ctx, s.db).QueryRowContext(ctx,
		`SELECT source, event_type, external_id, run_id, signal_name, dedupe_key, created_at
		 FROM workflow_correlation WHERE source = ? AND event_type = ? AND external_id = ? AND dedupe_key = ?`,
		source, eventType, externalID, dedupeKey,
	).Scan(&correlation.Source, &correlation.EventType, &correlation.ExternalID,
		&correlation.RunID, &correlation.SignalName, &correlation.DedupeKey, &createdAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Correlation{}, codedError(ErrorCodeCorrelationUnmatched, "no workflow waits for this provider event")
		}
		return Correlation{}, sqlite.Classify(fmt.Errorf("resolve correlation binding: %w", err))
	}
	parsed, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return Correlation{}, codedError(ErrorCodeStepFailed, "decode correlation binding: "+err.Error())
	}
	correlation.CreatedAt = parsed
	return correlation, nil
}

func (s *Store) DeleteCorrelation(ctx context.Context, source, eventType, externalID, dedupeKey string) error {
	result, err := sqlite.Conn(ctx, s.db).ExecContext(ctx,
		`DELETE FROM workflow_correlation WHERE source = ? AND event_type = ? AND external_id = ? AND dedupe_key = ?`,
		source, eventType, externalID, dedupeKey,
	)
	if err != nil {
		return sqlite.Classify(fmt.Errorf("delete correlation binding: %w", err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return sqlite.Classify(fmt.Errorf("delete correlation binding: %w", err))
	}
	if affected == 0 {
		return codedError(ErrorCodeCorrelationUnmatched, "no correlation binding to delete")
	}
	return nil
}

func (s *Store) ListCorrelationsByRun(ctx context.Context, runID string) ([]Correlation, error) {
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx,
		`SELECT source, event_type, external_id, run_id, signal_name, dedupe_key, created_at
		 FROM workflow_correlation WHERE run_id = ? ORDER BY created_at, event_type, external_id`,
		runID,
	)
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list correlation bindings: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var correlations []Correlation
	for rows.Next() {
		var correlation Correlation
		var createdAt string
		if err := rows.Scan(&correlation.Source, &correlation.EventType, &correlation.ExternalID,
			&correlation.RunID, &correlation.SignalName, &correlation.DedupeKey, &createdAt); err != nil {
			return nil, sqlite.Classify(fmt.Errorf("scan correlation binding: %w", err))
		}
		parsed, err := time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return nil, codedError(ErrorCodeStepFailed, "decode correlation binding: "+err.Error())
		}
		correlation.CreatedAt = parsed
		correlations = append(correlations, correlation)
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list correlation bindings: %w", err))
	}
	return correlations, nil
}
