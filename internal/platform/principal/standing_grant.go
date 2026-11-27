package principal

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

type StandingGrant struct {
	ID          string
	PrincipalID string
	AgentID     string
	ToolName    string
	GrantedBy   string
	GrantedAt   time.Time
	ExpiresAt   time.Time
}

type StandingGrantStore interface {
	Grant(ctx context.Context, grant *StandingGrant) (StandingGrant, error)
	Active(ctx context.Context, principalID, agentID, toolName string, now time.Time) (StandingGrant, bool, error)
	List(ctx context.Context, principalID string, now time.Time) ([]StandingGrant, error)
	Revoke(ctx context.Context, id string, at time.Time) (bool, error)
}

type sqliteStandingGrantStore struct {
	db *sql.DB
}

func NewStandingGrantStore(db *sql.DB) StandingGrantStore {
	return &sqliteStandingGrantStore{db: db}
}

func (s *sqliteStandingGrantStore) Grant(ctx context.Context, grant *StandingGrant) (StandingGrant, error) {
	if grant == nil {
		return StandingGrant{}, fault.New(ErrorCodeInvalidArgument, "standing grant must not be nil")
	}
	if strings.TrimSpace(grant.PrincipalID) == "" || strings.TrimSpace(grant.AgentID) == "" || strings.TrimSpace(grant.ToolName) == "" || strings.TrimSpace(grant.GrantedBy) == "" {
		return StandingGrant{}, fault.New(ErrorCodeInvalidArgument, "standing grant needs principal, persona, tool and granter")
	}
	if !grant.ExpiresAt.After(grant.GrantedAt) {
		return StandingGrant{}, fault.New(ErrorCodeInvalidArgument, "standing grant must expire after it is granted")
	}
	stored := *grant
	if stored.ID == "" {
		var raw [8]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return StandingGrant{}, fmt.Errorf("mint standing grant id: %w", err)
		}
		stored.ID = "sg_" + hex.EncodeToString(raw[:])
	}
	stored.GrantedAt = stored.GrantedAt.UTC()
	stored.ExpiresAt = stored.ExpiresAt.UTC()
	_, err := sqlite.Conn(ctx, s.db).ExecContext(ctx, `
INSERT INTO standing_grant (id, principal_id, agent_id, tool_name, granted_at, expires_at, granted_by)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		stored.ID, stored.PrincipalID, stored.AgentID, stored.ToolName,
		sqlite.FormatTime(stored.GrantedAt), sqlite.FormatTime(stored.ExpiresAt), stored.GrantedBy,
	)
	if err != nil {
		return StandingGrant{}, classify(fmt.Errorf("record standing grant: %w", err))
	}
	return stored, nil
}

func (s *sqliteStandingGrantStore) Active(ctx context.Context, principalID, agentID, toolName string, now time.Time) (StandingGrant, bool, error) {
	if principalID == "" || agentID == "" || toolName == "" {
		return StandingGrant{}, false, fault.New(ErrorCodeInvalidArgument, "standing grant lookup needs principal, persona and tool")
	}
	grants, err := s.query(ctx, `
SELECT id, principal_id, agent_id, tool_name, granted_at, expires_at, granted_by
FROM standing_grant
WHERE principal_id = ? AND agent_id = ? AND tool_name = ? AND revoked_at IS NULL`, principalID, agentID, toolName)
	if err != nil {
		return StandingGrant{}, false, err
	}
	for i := range grants {
		if grants[i].ExpiresAt.After(now) {
			return grants[i], true, nil
		}
	}
	return StandingGrant{}, false, nil
}

func (s *sqliteStandingGrantStore) List(ctx context.Context, principalID string, now time.Time) ([]StandingGrant, error) {
	query := `
SELECT id, principal_id, agent_id, tool_name, granted_at, expires_at, granted_by
FROM standing_grant
WHERE revoked_at IS NULL`
	var args []any
	if principalID != "" {
		query += ` AND principal_id = ?`
		args = append(args, principalID)
	}
	grants, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	grants = slices.DeleteFunc(grants, func(grant StandingGrant) bool { return !grant.ExpiresAt.After(now) })
	slices.SortFunc(grants, func(a, b StandingGrant) int {
		if c := a.ExpiresAt.Compare(b.ExpiresAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return grants, nil
}

func (s *sqliteStandingGrantStore) Revoke(ctx context.Context, id string, at time.Time) (bool, error) {
	if strings.TrimSpace(id) == "" {
		return false, fault.New(ErrorCodeInvalidArgument, "standing grant id must not be empty")
	}
	result, err := sqlite.Conn(ctx, s.db).ExecContext(ctx, `UPDATE standing_grant SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, sqlite.FormatTime(at.UTC()), id)
	if err != nil {
		return false, classify(fmt.Errorf("revoke standing grant: %w", err))
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, classify(fmt.Errorf("revoke standing grant: %w", err))
	}
	return rows == 1, nil
}

func (s *sqliteStandingGrantStore) query(ctx context.Context, query string, args ...any) ([]StandingGrant, error) {
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx, query, args...)
	if err != nil {
		return nil, classify(fmt.Errorf("read standing grants: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var grants []StandingGrant
	for rows.Next() {
		var grant StandingGrant
		var grantedAt, expiresAt string
		if err := rows.Scan(&grant.ID, &grant.PrincipalID, &grant.AgentID, &grant.ToolName, &grantedAt, &expiresAt, &grant.GrantedBy); err != nil {
			return nil, classify(fmt.Errorf("scan standing grant: %w", err))
		}
		if grant.GrantedAt, err = sqlite.ParseTime(grantedAt); err != nil {
			return nil, fmt.Errorf("parse standing grant time: %w", err)
		}
		if grant.ExpiresAt, err = sqlite.ParseTime(expiresAt); err != nil {
			return nil, fmt.Errorf("parse standing grant expiry: %w", err)
		}
		grants = append(grants, grant)
	}
	if err := rows.Err(); err != nil {
		return nil, classify(fmt.Errorf("read standing grants: %w", err))
	}
	return grants, nil
}
