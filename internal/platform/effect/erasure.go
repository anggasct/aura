package effect

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const (
	emailEffectMatch = `substr(?1, 1, 6) = 'email:' AND json_valid(request_json) AND (
		(provider = 'email' AND lower(json_extract(request_json, '$.to')) = substr(?1, 7))
		OR (provider = 'builtin-tools' AND operation = 'email_send' AND lower(json_extract(request_json, '$.arguments.to')) = substr(?1, 7)))`

	emailEffectIDs = `SELECT id FROM effect_intent WHERE ` + emailEffectMatch
)

type SessionIDsReader interface {
	SessionIDs(ctx context.Context, ownerID string, runSessions bool) ([]string, error)
}

type EmailEffectEraser struct {
	db *sql.DB
}

func NewEmailEffectEraser(db *sql.DB) *EmailEffectEraser {
	return &EmailEffectEraser{db: db}
}

func (e *EmailEffectEraser) ErasePrincipal(ctx context.Context, state *principal.ErasureState) error {
	if e == nil || e.db == nil {
		return fault.New(ErrorCodeInvalidArgument, "effect: database must not be nil")
	}
	if state == nil {
		return fault.New(ErrorCodeInvalidArgument, "effect: erasure state must not be nil")
	}
	scope := state.Scope()
	conn := sqlite.Conn(ctx, e.db)
	var count int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM effect_intent WHERE `+emailEffectMatch, scope.PrincipalID).Scan(&count); err != nil {
		return sqlite.Classify(fmt.Errorf("count principal data: %w", err))
	}
	if err := state.Record(principal.ErasureLineEmailEffects, count); err != nil {
		return err
	}
	if !scope.Apply || count == 0 {
		return nil
	}
	for _, stmt := range []string{
		`DELETE FROM effect_approval WHERE intent_id IN (` + emailEffectIDs + `)`,
		`DELETE FROM effect_intent WHERE id IN (` + emailEffectIDs + `)`,
	} {
		if _, err := conn.ExecContext(ctx, stmt, scope.PrincipalID); err != nil {
			return sqlite.Classify(fmt.Errorf("erase principal data: %w", err))
		}
	}
	return nil
}

type SessionApprovalEraser struct {
	db          *sql.DB
	sessions    SessionIDsReader
	runSessions bool
}

func NewSessionApprovalEraser(db *sql.DB, sessions SessionIDsReader, runSessions bool) (*SessionApprovalEraser, error) {
	if db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "effect: database must not be nil")
	}
	if sessions == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "effect: session reader must not be nil")
	}
	return &SessionApprovalEraser{
		db:          db,
		sessions:    sessions,
		runSessions: runSessions,
	}, nil
}

func (e *SessionApprovalEraser) ErasePrincipal(ctx context.Context, state *principal.ErasureState) error {
	if e == nil || e.db == nil {
		return fault.New(ErrorCodeInvalidArgument, "effect: database must not be nil")
	}
	if e.sessions == nil {
		return fault.New(ErrorCodeInvalidArgument, "effect: session reader must not be nil")
	}
	if state == nil {
		return fault.New(ErrorCodeInvalidArgument, "effect: erasure state must not be nil")
	}
	scope := state.Scope()
	if !scope.Apply {
		return nil
	}
	ids, err := e.sessions.SessionIDs(ctx, scope.PrincipalID, e.runSessions)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return fmt.Errorf("encode session ids: %w", err)
	}
	if _, err := sqlite.Conn(ctx, e.db).ExecContext(ctx,
		`DELETE FROM effect_approval WHERE intent_id IN (SELECT id FROM effect_intent WHERE session_id IN (SELECT value FROM json_each(?)))`,
		string(encoded),
	); err != nil {
		return sqlite.Classify(fmt.Errorf("erase principal data: %w", err))
	}
	return nil
}
