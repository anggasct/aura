package memory

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
		return nil, fault.New(ErrorCodeInvalidArgument, "db must not be nil")
	}
	return &Rekeyer{db: db}, nil
}

func (r *Rekeyer) RekeyOwner(ctx context.Context, state *principal.RekeyState, move principal.OwnerMove) error {
	if r == nil || r.db == nil {
		return fault.New(ErrorCodeInvalidArgument, "db must not be nil")
	}
	if state == nil {
		return fault.New(ErrorCodeInvalidArgument, "state must not be nil")
	}
	result, err := sqlite.Conn(ctx, r.db).ExecContext(ctx, `UPDATE memory_document SET owner_id = ? WHERE owner_id = ?`, move.To, move.From)
	if err != nil {
		return sqlite.Classify(err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return sqlite.Classify(err)
	}
	return state.Record(principal.RekeyLineMemoryDocuments, affected)
}
