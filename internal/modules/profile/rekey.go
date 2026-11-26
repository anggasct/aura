package profile

import (
	"context"
	"database/sql"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

type Rekeyer struct {
	db *sql.DB
}

func NewRekeyer(db *sql.DB) (*Rekeyer, error) {
	if db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	return &Rekeyer{db: db}, nil
}

func (r *Rekeyer) RekeyOwner(ctx context.Context, state *principal.RekeyState, move principal.OwnerMove) error {
	if r == nil || r.db == nil {
		return fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if state == nil {
		return fault.New(ErrorCodeInvalidArgument, "rekey state must not be nil")
	}
	conn := sqlite.Conn(ctx, r.db)
	result, err := conn.ExecContext(ctx, `UPDATE profile_fact SET owner_id = ?1 WHERE owner_id = ?2 AND NOT EXISTS (
		SELECT 1 FROM profile_fact AS target
		WHERE target.owner_id = ?1 AND target.category = profile_fact.category
			AND target.fact_key = profile_fact.fact_key AND target.value_digest = profile_fact.value_digest)`,
		move.To, move.From,
	)
	if err != nil {
		return sqlite.Classify(err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return sqlite.Classify(err)
	}
	if err := state.Record(principal.RekeyLineProfileFacts, affected); err != nil {
		return err
	}
	var conflicts int64
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM profile_fact WHERE owner_id = ?`, move.From).Scan(&conflicts); err != nil {
		return sqlite.Classify(err)
	}
	return state.Record(principal.RekeyLineProfileFactConflicts, conflicts)
}
