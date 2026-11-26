package turnstore

import (
	"context"
	"database/sql"

	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

type SessionRekeyer struct {
	db *sql.DB
}

func NewSessionRekeyer(db *sql.DB) *SessionRekeyer {
	return &SessionRekeyer{db: db}
}

func (r *SessionRekeyer) RekeyOwner(ctx context.Context, state *principal.RekeyState, move principal.OwnerMove) error {
	if r == nil || r.db == nil {
		return errNilArgument("db")
	}
	if state == nil {
		return errNilArgument("state")
	}
	result, err := sqlite.Conn(ctx, r.db).ExecContext(ctx, `UPDATE session SET owner_id = ? WHERE owner_id = ?`, move.To, move.From)
	if err != nil {
		return classify(err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return classify(err)
	}
	return state.Record(principal.RekeyLineSessions, affected)
}
