package workspace

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/anggasct/aura/internal/platform/principal"
)

type SessionLister interface {
	SessionIDs(ctx context.Context, ownerID string, runSessions bool) ([]string, error)
}

type TaintEraser struct {
	store    *TaintStore
	sessions SessionLister
}

func NewTaintEraser(db *sql.DB, sessions SessionLister) *TaintEraser {
	return &TaintEraser{
		store:    NewTaintStore(db),
		sessions: sessions,
	}
}

func (e *TaintEraser) ErasePrincipal(ctx context.Context, state *principal.ErasureState) error {
	if e == nil || e.store == nil || e.sessions == nil || state == nil {
		return nil
	}
	scope := state.Scope()
	if !scope.Apply {
		return nil
	}
	normal, err := e.sessions.SessionIDs(ctx, scope.PrincipalID, false)
	if err != nil {
		return fmt.Errorf("workspace: list principal sessions: %w", err)
	}
	runs, err := e.sessions.SessionIDs(ctx, scope.PrincipalID, true)
	if err != nil {
		return fmt.Errorf("workspace: list principal run sessions: %w", err)
	}
	all := make([]string, 0, len(normal)+len(runs))
	all = append(all, normal...)
	all = append(all, runs...)
	if len(all) == 0 {
		return nil
	}
	_, err = e.store.NullSessionIDs(ctx, all)
	return err
}
