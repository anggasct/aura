package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	APIKeyClassOwner   = "owner"
	APIKeyClassTrusted = "trusted"
	APIKeyClassPublic  = "public"
)

const (
	apiKeyIDLength     = 12
	apiKeyDigestLength = 64
)

type APIKey struct {
	ID        string
	KeySHA256 string
	PersonaID string
	Class     string
	Label     string
	CreatedAt time.Time
	RevokedAt time.Time
}

func (k *APIKey) Active() bool {
	return k.RevokedAt.IsZero()
}

type APIKeyStore interface {
	Create(ctx context.Context, key *APIKey) error
	GetByID(ctx context.Context, id string) (APIKey, error)
	List(ctx context.Context) ([]APIKey, error)
	Revoke(ctx context.Context, id string, at time.Time) (bool, error)
}

type sqliteAPIKeyStore struct {
	db *sql.DB
}

func NewAPIKeyStore(db *sql.DB) APIKeyStore {
	return &sqliteAPIKeyStore{db: db}
}

const channelKeyColumns = `id, key_sha256, persona_id, class, label, created_at, COALESCE(revoked_at, '')`

func (s *sqliteAPIKeyStore) Create(ctx context.Context, key *APIKey) error {
	if key == nil {
		return errNilArgument("api key")
	}
	if len(key.ID) != apiKeyIDLength || len(key.KeySHA256) != apiKeyDigestLength {
		return Errorf(ErrorCodeAPIKeyInvalid, "api key id and digest have the wrong length")
	}
	if strings.TrimSpace(key.PersonaID) == "" || strings.TrimSpace(key.Label) == "" {
		return Errorf(ErrorCodeAPIKeyInvalid, "api key needs a persona and a label")
	}
	if key.CreatedAt.IsZero() {
		return Errorf(ErrorCodeAPIKeyInvalid, "api key needs a creation time")
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO api_key (id, key_sha256, persona_id, class, label, created_at, revoked_at)
VALUES (?, ?, ?, ?, ?, ?, NULL)`,
		key.ID, key.KeySHA256, key.PersonaID, key.Class, key.Label, formatTime(key.CreatedAt))
	if err != nil {
		switch {
		case isConstraintUnique(err):
			return codedError(ErrorCodeAPIKeyConflict, "api key id or digest already exists", err)
		case isConstraintCheck(err):
			return codedError(ErrorCodeAPIKeyInvalid, "api key violates a storage constraint", err)
		}
		return classifyBusy(fmt.Errorf("create api key: %w", err))
	}
	return nil
}

func (s *sqliteAPIKeyStore) GetByID(ctx context.Context, id string) (APIKey, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+channelKeyColumns+` FROM api_key WHERE id = ?`, id)
	key, err := scanAPIKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return APIKey{}, Errorf(ErrorCodeAPIKeyNotFound, "api key does not exist")
	}
	if err != nil {
		return APIKey{}, classifyBusy(fmt.Errorf("read api key: %w", err))
	}
	return key, nil
}

func (s *sqliteAPIKeyStore) List(ctx context.Context) ([]APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+channelKeyColumns+` FROM api_key ORDER BY created_at, id`)
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("list api keys: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var keys []APIKey
	for rows.Next() {
		key, err := scanAPIKey(rows)
		if err != nil {
			return nil, fmt.Errorf("list api keys: %w", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, classifyBusy(fmt.Errorf("list api keys: %w", err))
	}
	return keys, nil
}

func (s *sqliteAPIKeyStore) Revoke(ctx context.Context, id string, at time.Time) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE api_key SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, formatTime(at), id)
	if err != nil {
		return false, classifyBusy(fmt.Errorf("revoke api key: %w", err))
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("revoke api key: %w", err)
	}
	if changed == 1 {
		return true, nil
	}
	if !rowExists(ctx, s.db, `SELECT 1 FROM api_key WHERE id = ?`, id) {
		return false, Errorf(ErrorCodeAPIKeyNotFound, "api key does not exist")
	}
	return false, nil
}

func scanAPIKey(row rowScanner) (APIKey, error) {
	var key APIKey
	var created, revoked string
	if err := row.Scan(&key.ID, &key.KeySHA256, &key.PersonaID, &key.Class, &key.Label, &created, &revoked); err != nil {
		return APIKey{}, err
	}
	var err error
	if key.CreatedAt, err = parseTime(created); err != nil {
		return APIKey{}, fmt.Errorf("parse api key creation time: %w", err)
	}
	if revoked != "" {
		if key.RevokedAt, err = parseTime(revoked); err != nil {
			return APIKey{}, fmt.Errorf("parse api key revocation time: %w", err)
		}
	}
	return key, nil
}
