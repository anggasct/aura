package memory

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

type Eraser struct {
	db *sql.DB
}

func NewEraser(db *sql.DB) (*Eraser, error) {
	if db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "db must not be nil")
	}
	return &Eraser{db: db}, nil
}

func (e *Eraser) ErasePrincipal(ctx context.Context, state *principal.ErasureState) error {
	if e == nil || e.db == nil {
		return fault.New(ErrorCodeInvalidArgument, "db must not be nil")
	}
	if state == nil {
		return fault.New(ErrorCodeInvalidArgument, "state must not be nil")
	}
	scope := state.Scope()
	conn := sqlite.Conn(ctx, e.db)
	var count int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_document WHERE owner_id = ?`, scope.PrincipalID).Scan(&count); err != nil {
		return sqlite.Classify(fmt.Errorf("count principal data: %w", err))
	}
	if err := state.Record(principal.ErasureLineMemoryDocuments, count); err != nil {
		return err
	}
	if !scope.Apply || count == 0 {
		return nil
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM memory_document WHERE owner_id = ?`, scope.PrincipalID); err != nil {
		return sqlite.Classify(fmt.Errorf("erase principal data: %w", err))
	}
	return nil
}
