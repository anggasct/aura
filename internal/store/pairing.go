package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const AccessOwnerActionEventKind = "access.owner_action.v1"

const AccessActionPairingInvalid = "pairing_invalid"

const (
	PairingIssued  = "issued"
	PairingPending = "pending"
)

type PairingRequest struct {
	PrincipalID string
	Account     string
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

type PairingIssue struct {
	CodeSHA256    string
	PrincipalID   string
	Account       string
	Now           time.Time
	TTL           time.Duration
	Cooldown      time.Duration
	MaxPerAccount int
}

type PrincipalGrant struct {
	PrincipalID string
	Account     string
	Class       string
	GrantedBy   string
	GrantedAt   time.Time
}

type PairingStore interface {
	Issue(ctx context.Context, issue *PairingIssue) (string, error)
	Approve(ctx context.Context, channel, codeSHA256, grantedBy string, now time.Time) (PairingRequest, bool, error)
	Pending(ctx context.Context, now time.Time) ([]PairingRequest, error)
	ClearPending(ctx context.Context) (int64, error)
	FailedApprovals(ctx context.Context, channel string, since time.Time) ([]time.Time, error)
}

type PrincipalGrantStore interface {
	Active(ctx context.Context, principalID, account string) (bool, error)
	List(ctx context.Context) ([]PrincipalGrant, error)
	Revoke(ctx context.Context, principalID string, at time.Time) ([]string, error)
}

type sqlitePairingStore struct {
	db *sql.DB
}

func NewPairingStore(db *sql.DB) PairingStore {
	return &sqlitePairingStore{db: db}
}

func NewPrincipalGrantStore(db *sql.DB) PrincipalGrantStore {
	return &sqlitePairingStore{db: db}
}

func (s *sqlitePairingStore) Issue(ctx context.Context, issue *PairingIssue) (string, error) {
	if issue == nil || issue.CodeSHA256 == "" || issue.PrincipalID == "" || issue.Account == "" || issue.TTL <= 0 || issue.MaxPerAccount <= 0 {
		return "", Errorf(ErrorCodeInvalidArgument, "pairing issue needs code, principal, account, lifetime and cap")
	}
	now := issue.Now.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", classifyBusy(fmt.Errorf("begin pairing issue: %w", err))
	}
	defer func() { _ = tx.Rollback() }()
	pending, err := scanPairingRequests(ctx, tx, `SELECT principal_id, account, created_at, expires_at, code_sha256 FROM pairing_request`)
	if err != nil {
		return "", err
	}
	var stale []string
	open := 0
	for i := range pending {
		row := &pending[i]
		switch {
		case !row.ExpiresAt.After(now):
			stale = append(stale, row.code)
		case row.Account != issue.Account:
		case row.PrincipalID == issue.PrincipalID:
			if now.Sub(row.CreatedAt) < issue.Cooldown {
				return PairingPending, nil
			}
			stale = append(stale, row.code)
		default:
			open++
		}
	}
	if open >= issue.MaxPerAccount {
		return PairingPending, nil
	}
	for _, code := range stale {
		if _, err := tx.ExecContext(ctx, `DELETE FROM pairing_request WHERE code_sha256 = ?`, code); err != nil {
			return "", classifyBusy(fmt.Errorf("drop stale pairing request: %w", err))
		}
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO pairing_request (code_sha256, principal_id, account, created_at, expires_at)
VALUES (?, ?, ?, ?, ?)`,
		issue.CodeSHA256, issue.PrincipalID, issue.Account, formatTime(now), formatTime(now.Add(issue.TTL)),
	); err != nil {
		return "", classifyBusy(fmt.Errorf("record pairing request: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return "", classifyBusy(fmt.Errorf("commit pairing request: %w", err))
	}
	return PairingIssued, nil
}

func (s *sqlitePairingStore) Approve(ctx context.Context, channel, codeSHA256, grantedBy string, now time.Time) (PairingRequest, bool, error) {
	if channel == "" {
		return PairingRequest{}, false, Errorf(ErrorCodeInvalidArgument, "pairing approval needs a channel")
	}
	return s.approve(ctx, channel+":", codeSHA256, grantedBy, now)
}

func (s *sqlitePairingStore) approve(ctx context.Context, principalPrefix, codeSHA256, grantedBy string, now time.Time) (PairingRequest, bool, error) {
	if codeSHA256 == "" || grantedBy == "" {
		return PairingRequest{}, false, Errorf(ErrorCodeInvalidArgument, "pairing approval needs a code and a granter")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PairingRequest{}, false, classifyBusy(fmt.Errorf("begin pairing approval: %w", err))
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := scanPairingRequests(ctx, tx, `SELECT principal_id, account, created_at, expires_at, code_sha256 FROM pairing_request WHERE code_sha256 = ?`, codeSHA256)
	if err != nil {
		return PairingRequest{}, false, err
	}
	if len(rows) == 0 || !rows[0].ExpiresAt.After(now) || !strings.HasPrefix(rows[0].PrincipalID, principalPrefix) {
		return PairingRequest{}, false, nil
	}
	request := rows[0].PairingRequest
	at := formatTime(now.UTC())
	if _, err := tx.ExecContext(ctx, `DELETE FROM pairing_request WHERE code_sha256 = ?`, codeSHA256); err != nil {
		return PairingRequest{}, false, classifyBusy(fmt.Errorf("consume pairing request: %w", err))
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO principal_grant (principal_id, account, class, granted_at, granted_by)
VALUES (?, ?, 'trusted', ?, ?)
ON CONFLICT(principal_id, account) DO UPDATE SET
    granted_at = excluded.granted_at,
    granted_by = excluded.granted_by,
    revoked_at = NULL`,
		request.PrincipalID, request.Account, at, grantedBy,
	); err != nil {
		return PairingRequest{}, false, classifyBusy(fmt.Errorf("record principal grant: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return PairingRequest{}, false, classifyBusy(fmt.Errorf("commit pairing approval: %w", err))
	}
	return request, true, nil
}

func (s *sqlitePairingStore) Pending(ctx context.Context, now time.Time) ([]PairingRequest, error) {
	rows, err := scanPairingRequests(ctx, s.db, `SELECT principal_id, account, created_at, expires_at, code_sha256 FROM pairing_request`)
	if err != nil {
		return nil, err
	}
	pending := make([]PairingRequest, 0, len(rows))
	for i := range rows {
		if rows[i].ExpiresAt.After(now) {
			pending = append(pending, rows[i].PairingRequest)
		}
	}
	slices.SortFunc(pending, func(a, b PairingRequest) int {
		if c := strings.Compare(a.Account, b.Account); c != 0 {
			return c
		}
		return a.ExpiresAt.Compare(b.ExpiresAt)
	})
	return pending, nil
}

func (s *sqlitePairingStore) ClearPending(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM pairing_request`)
	if err != nil {
		return 0, classifyBusy(fmt.Errorf("clear pairing requests: %w", err))
	}
	return result.RowsAffected()
}

func (s *sqlitePairingStore) FailedApprovals(ctx context.Context, channel string, since time.Time) ([]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT created_at FROM runtime_event
WHERE kind = ? AND json_extract(payload_json, '$.action') = ? AND json_extract(payload_json, '$.channel') = ?`,
		AccessOwnerActionEventKind, AccessActionPairingInvalid, channel)
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("read failed pairing approvals: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var failures []time.Time
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("scan failed pairing approval: %w", err)
		}
		at, err := parseTime(raw)
		if err != nil {
			return nil, fmt.Errorf("parse failed pairing approval time: %w", err)
		}
		if !at.Before(since) {
			failures = append(failures, at)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read failed pairing approvals: %w", err)
	}
	slices.SortFunc(failures, time.Time.Compare)
	return failures, nil
}

func (s *sqlitePairingStore) Active(ctx context.Context, principalID, account string) (bool, error) {
	var found int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM principal_grant WHERE principal_id = ? AND account = ? AND revoked_at IS NULL`, principalID, account).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, classifyBusy(fmt.Errorf("read principal grant: %w", err))
	}
	return true, nil
}

func (s *sqlitePairingStore) List(ctx context.Context) ([]PrincipalGrant, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT principal_id, account, class, granted_by, granted_at FROM principal_grant
WHERE revoked_at IS NULL ORDER BY principal_id, account`)
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("read principal grants: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var grants []PrincipalGrant
	for rows.Next() {
		var grant PrincipalGrant
		var grantedAt string
		if err := rows.Scan(&grant.PrincipalID, &grant.Account, &grant.Class, &grant.GrantedBy, &grantedAt); err != nil {
			return nil, fmt.Errorf("scan principal grant: %w", err)
		}
		if grant.GrantedAt, err = parseTime(grantedAt); err != nil {
			return nil, fmt.Errorf("parse principal grant time: %w", err)
		}
		grants = append(grants, grant)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read principal grants: %w", err)
	}
	return grants, nil
}

func (s *sqlitePairingStore) Revoke(ctx context.Context, principalID string, at time.Time) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("begin principal grant revoke: %w", err))
	}
	defer func() { _ = tx.Rollback() }()
	accounts, err := activeGrantAccounts(ctx, tx, principalID)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE principal_grant SET revoked_at = ? WHERE principal_id = ? AND revoked_at IS NULL`, formatTime(at.UTC()), principalID); err != nil {
		return nil, classifyBusy(fmt.Errorf("revoke principal grant: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return nil, classifyBusy(fmt.Errorf("commit principal grant revoke: %w", err))
	}
	return accounts, nil
}

func activeGrantAccounts(ctx context.Context, q pairingQuerier, principalID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT account FROM principal_grant WHERE principal_id = ? AND revoked_at IS NULL ORDER BY account`, principalID)
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("read principal grants: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var accounts []string
	for rows.Next() {
		var account string
		if err := rows.Scan(&account); err != nil {
			return nil, fmt.Errorf("scan principal grant: %w", err)
		}
		accounts = append(accounts, account)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read principal grants: %w", err)
	}
	return accounts, nil
}

type pairingRow struct {
	PairingRequest
	code string
}

type pairingQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func scanPairingRequests(ctx context.Context, q pairingQuerier, query string, args ...any) ([]pairingRow, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("read pairing requests: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var requests []pairingRow
	for rows.Next() {
		var row pairingRow
		var createdAt, expiresAt string
		if err := rows.Scan(&row.PrincipalID, &row.Account, &createdAt, &expiresAt, &row.code); err != nil {
			return nil, fmt.Errorf("scan pairing request: %w", err)
		}
		if row.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, fmt.Errorf("parse pairing request time: %w", err)
		}
		if row.ExpiresAt, err = parseTime(expiresAt); err != nil {
			return nil, fmt.Errorf("parse pairing request expiry: %w", err)
		}
		requests = append(requests, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read pairing requests: %w", err)
	}
	return requests, nil
}

type PrincipalActivity struct {
	PrincipalID string
	Sessions    int
	LastActive  time.Time
}

func ListPrincipalActivity(ctx context.Context, db *sql.DB) ([]PrincipalActivity, error) {
	rows, err := db.QueryContext(ctx, `SELECT owner_id, updated_at FROM session WHERE owner_id LIKE '%:%'`)
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("read principal sessions: %w", err))
	}
	defer func() { _ = rows.Close() }()
	byPrincipal := map[string]*PrincipalActivity{}
	for rows.Next() {
		var owner, updatedAt string
		if err := rows.Scan(&owner, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan principal session: %w", err)
		}
		at, err := parseTime(updatedAt)
		if err != nil {
			return nil, fmt.Errorf("parse principal session time: %w", err)
		}
		activity, ok := byPrincipal[owner]
		if !ok {
			activity = &PrincipalActivity{PrincipalID: owner}
			byPrincipal[owner] = activity
		}
		activity.Sessions++
		if at.After(activity.LastActive) {
			activity.LastActive = at
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read principal sessions: %w", err)
	}
	activities := make([]PrincipalActivity, 0, len(byPrincipal))
	for _, activity := range byPrincipal {
		activities = append(activities, *activity)
	}
	slices.SortFunc(activities, func(a, b PrincipalActivity) int { return strings.Compare(a.PrincipalID, b.PrincipalID) })
	return activities, nil
}
