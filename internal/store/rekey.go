package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

type OwnerMove struct {
	From string
	To   string
}

type OwnerRekeyReport struct {
	Sessions             int64
	MemoryDocuments      int64
	ProfileFacts         int64
	ProfileFactConflicts int64
}

func (r OwnerRekeyReport) Moved() int64 {
	return r.Sessions + r.MemoryDocuments + r.ProfileFacts
}

// RekeyOwners runs the same statements for a dry run and rolls them back, so
// the preview counts are exactly what an applied run would move, including
// profile facts that only collide after an earlier move in the same batch.
func RekeyOwners(ctx context.Context, db *sql.DB, moves []OwnerMove, apply bool) (OwnerRekeyReport, error) {
	if db == nil {
		return OwnerRekeyReport{}, Errorf(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if err := validateOwnerMoves(moves); err != nil {
		return OwnerRekeyReport{}, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return OwnerRekeyReport{}, classifyBusy(fmt.Errorf("rekey owners: %w", err))
	}
	defer func() { _ = tx.Rollback() }()
	var report OwnerRekeyReport
	for _, move := range moves {
		if err := rekeyOwner(ctx, tx, move, &report); err != nil {
			return OwnerRekeyReport{}, classifyBusy(fmt.Errorf("rekey owners: %w", err))
		}
	}
	if !apply {
		return report, nil
	}
	if err := tx.Commit(); err != nil {
		return OwnerRekeyReport{}, classifyBusy(fmt.Errorf("rekey owners: %w", err))
	}
	return report, nil
}

func validateOwnerMoves(moves []OwnerMove) error {
	targets := make(map[string]struct{}, len(moves))
	sources := make(map[string]struct{}, len(moves))
	for _, move := range moves {
		if strings.TrimSpace(move.From) == "" || strings.TrimSpace(move.To) == "" || move.From == move.To {
			return Errorf(ErrorCodeInvalidArgument, "owner move needs distinct non-empty source and target")
		}
		if _, dup := sources[move.From]; dup {
			return Errorf(ErrorCodeInvalidArgument, "owner move source is listed more than once")
		}
		sources[move.From] = struct{}{}
		targets[move.To] = struct{}{}
	}
	for source := range sources {
		if _, chained := targets[source]; chained {
			return Errorf(ErrorCodeInvalidArgument, "owner move source is also a target")
		}
	}
	return nil
}

func rekeyOwner(ctx context.Context, tx *sql.Tx, move OwnerMove, report *OwnerRekeyReport) error {
	for _, step := range []struct {
		query string
		count *int64
	}{
		{`UPDATE session SET owner_id = ? WHERE owner_id = ?`, &report.Sessions},
		{`UPDATE memory_document SET owner_id = ? WHERE owner_id = ?`, &report.MemoryDocuments},
		{`UPDATE profile_fact SET owner_id = ?1 WHERE owner_id = ?2 AND NOT EXISTS (
			SELECT 1 FROM profile_fact AS target
			WHERE target.owner_id = ?1 AND target.category = profile_fact.category
				AND target.fact_key = profile_fact.fact_key AND target.value_digest = profile_fact.value_digest)`, &report.ProfileFacts},
	} {
		result, err := tx.ExecContext(ctx, step.query, move.To, move.From)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		*step.count += affected
	}
	var conflicts int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM profile_fact WHERE owner_id = ?`, move.From).Scan(&conflicts); err != nil {
		return err
	}
	report.ProfileFactConflicts += conflicts
	return nil
}
