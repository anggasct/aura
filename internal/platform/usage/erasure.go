package usage

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

type UsageLinkEraser struct {
	db *sql.DB
}

func NewUsageLinkEraser(db *sql.DB) *UsageLinkEraser {
	return &UsageLinkEraser{db: db}
}

func (e *UsageLinkEraser) ErasePrincipal(ctx context.Context, state *principal.ErasureState) error {
	if e == nil || e.db == nil {
		return fault.New(ErrorCodeInvalidArgument, "usage: database must not be nil")
	}
	if state == nil {
		return fault.New(ErrorCodeInvalidArgument, "usage: erasure state must not be nil")
	}
	scope := state.Scope()
	conn := sqlite.Conn(ctx, e.db)
	var count int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_reservation WHERE principal_id = ?`, scope.PrincipalID).Scan(&count); err != nil {
		return sqlite.Classify(fmt.Errorf("count principal data: %w", err))
	}
	if err := state.Record(principal.ErasureLineUsageLinks, count); err != nil {
		return err
	}
	if !scope.Apply || count == 0 {
		return nil
	}
	if _, err := conn.ExecContext(ctx, `UPDATE usage_reservation SET principal_id = NULL WHERE principal_id = ?`, scope.PrincipalID); err != nil {
		return sqlite.Classify(fmt.Errorf("erase principal data: %w", err))
	}
	return nil
}
