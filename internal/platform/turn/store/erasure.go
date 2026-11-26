package turnstore

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

type SessionEraser struct {
	db          *sql.DB
	runSessions bool
}

func NewSessionEraser(db *sql.DB, runSessions bool) *SessionEraser {
	return &SessionEraser{db: db, runSessions: runSessions}
}

func (e *SessionEraser) ErasePrincipal(ctx context.Context, state *principal.ErasureState) error {
	if e == nil || e.db == nil {
		return errNilArgument("db")
	}
	if state == nil {
		return errNilArgument("state")
	}
	scope := state.Scope()
	line := principal.ErasureLineSessions
	countQuery := `SELECT COUNT(*) FROM session WHERE owner_id = ? AND id NOT GLOB 'wf-*'`
	deleteQuery := `DELETE FROM session WHERE owner_id = ? AND id NOT GLOB 'wf-*'`
	if e.runSessions {
		line = principal.ErasureLineRunSessions
		countQuery = `SELECT COUNT(*) FROM session WHERE owner_id = ? AND id GLOB 'wf-*'`
		deleteQuery = `DELETE FROM session WHERE owner_id = ? AND id GLOB 'wf-*'`
	}
	conn := sqlite.Conn(ctx, e.db)
	var count int
	if err := conn.QueryRowContext(ctx, countQuery, scope.PrincipalID).Scan(&count); err != nil {
		return classify(fmt.Errorf("count principal data: %w", err))
	}
	if err := state.Record(line, count); err != nil {
		return err
	}
	if !scope.Apply || count == 0 {
		return nil
	}
	if _, err := conn.ExecContext(ctx, deleteQuery, scope.PrincipalID); err != nil {
		return classify(fmt.Errorf("erase principal data: %w", err))
	}
	return nil
}
