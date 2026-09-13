package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

const (
	SkillStateQuarantined = "quarantined"
	SkillStateActive      = "active"
	SkillStateDisabled    = "disabled"
	SkillStateRejected    = "rejected"
	SkillStateConflict    = "conflict"
)

const defaultSkillListLimit = 50
const maxSkillListLimit = 1000

type SkillRow struct {
	ID         string
	Name       string
	Origin     string
	Digest     string
	State      string
	Validation string
	Requested  string
	Granted    string
	ReviewedAt *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type SkillStore interface {
	UpsertScan(ctx context.Context, row *SkillRow) (bool, error)
	GetSkill(ctx context.Context, id string) (SkillRow, error)
	ListSkillsByState(ctx context.Context, state string, limit int) ([]SkillRow, error)
	AcceptSkill(ctx context.Context, id, digest, granted string) error
	RejectSkill(ctx context.Context, id string) error
}

type sqliteSkillStore struct {
	db *sql.DB
}

func NewSkillStore(db *sql.DB) SkillStore {
	return &sqliteSkillStore{db: db}
}

func validSkillState(state string) bool {
	switch state {
	case SkillStateQuarantined, SkillStateActive, SkillStateDisabled, SkillStateRejected, SkillStateConflict:
		return true
	default:
		return false
	}
}

func (s *sqliteSkillStore) UpsertScan(ctx context.Context, row *SkillRow) (bool, error) {
	if ctx == nil {
		return false, errNilArgument("ctx")
	}
	if row == nil {
		return false, errNilArgument("row")
	}
	if row.ID == "" || row.Name == "" || row.Origin == "" || row.Digest == "" {
		return false, Errorf(ErrorCodeSkillInvalid, "skill identity fields must not be empty")
	}
	if !validSkillState(row.State) {
		return false, Errorf(ErrorCodeSkillInvalid, "skill state is not valid")
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"validation", row.Validation},
		{"requested", row.Requested},
		{"granted", row.Granted},
	} {
		if !json.Valid([]byte(field.value)) {
			return false, Errorf(ErrorCodeSkillInvalid, "skill %s must be valid JSON", field.name)
		}
	}
	now := formatTime(time.Now().UTC())
	created := now
	if !row.CreatedAt.IsZero() {
		created = formatTime(row.CreatedAt)
	}
	result, err := s.db.ExecContext(ctx, `
INSERT INTO skill_package (id, canonical_name, origin_json, content_digest, state, validation_json, requested_capabilities_json, granted_capabilities_json, reviewed_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO NOTHING
`, row.ID, row.Name, row.Origin, row.Digest, row.State, row.Validation, row.Requested, row.Granted, nullSkillTime(row.ReviewedAt), created, now)
	if err != nil {
		return false, classifyBusy(codedError(ErrorCodeStorageUnavailable, "record skill scan", err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, codedError(ErrorCodeStorageUnavailable, "record skill scan", err)
	}
	return affected == 1, nil
}

func (s *sqliteSkillStore) GetSkill(ctx context.Context, id string) (SkillRow, error) {
	if ctx == nil {
		return SkillRow{}, errNilArgument("ctx")
	}
	if id == "" {
		return SkillRow{}, Errorf(ErrorCodeInvalidArgument, "skill id must not be empty")
	}
	var row SkillRow
	var reviewed, created, updated sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT id, canonical_name, origin_json, content_digest, state, validation_json, requested_capabilities_json, granted_capabilities_json, reviewed_at, created_at, updated_at
FROM skill_package WHERE id = ?
`, id).Scan(&row.ID, &row.Name, &row.Origin, &row.Digest, &row.State, &row.Validation, &row.Requested, &row.Granted, &reviewed, &created, &updated)
	if err == sql.ErrNoRows {
		return SkillRow{}, Errorf(ErrorCodeSkillNotFound, "skill is not registered")
	}
	if err != nil {
		return SkillRow{}, classifyBusy(codedError(ErrorCodeStorageUnavailable, "read skill", err))
	}
	if err := row.bindTimes(reviewed, created, updated); err != nil {
		return SkillRow{}, err
	}
	return row, nil
}

func (s *sqliteSkillStore) ListSkillsByState(ctx context.Context, state string, limit int) ([]SkillRow, error) {
	if ctx == nil {
		return nil, errNilArgument("ctx")
	}
	if !validSkillState(state) {
		return nil, Errorf(ErrorCodeSkillInvalid, "skill state is not valid")
	}
	if limit < 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "limit must not be negative")
	}
	if limit == 0 {
		limit = defaultSkillListLimit
	}
	if limit > maxSkillListLimit {
		limit = maxSkillListLimit
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, canonical_name, origin_json, content_digest, state, validation_json, requested_capabilities_json, granted_capabilities_json, reviewed_at, created_at, updated_at
FROM skill_package WHERE state = ? ORDER BY updated_at DESC, id ASC LIMIT ?
`, state, limit)
	if err != nil {
		return nil, classifyBusy(codedError(ErrorCodeStorageUnavailable, "list skills", err))
	}
	defer func() { _ = rows.Close() }()
	out := make([]SkillRow, 0, limit)
	for rows.Next() {
		var row SkillRow
		var reviewed, created, updated sql.NullString
		if err := rows.Scan(&row.ID, &row.Name, &row.Origin, &row.Digest, &row.State, &row.Validation, &row.Requested, &row.Granted, &reviewed, &created, &updated); err != nil {
			return nil, codedError(ErrorCodeStorageUnavailable, "list skills", err)
		}
		if err := row.bindTimes(reviewed, created, updated); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, codedError(ErrorCodeStorageUnavailable, "list skills", err)
	}
	return out, nil
}

func nullSkillTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return formatTime(*value)
}

func (row *SkillRow) bindTimes(reviewed, created, updated sql.NullString) error {
	if reviewed.Valid && reviewed.String != "" {
		parsed, err := parseTime(reviewed.String)
		if err != nil {
			return Errorf(ErrorCodeSkillInvalid, "skill reviewed_at is not valid")
		}
		row.ReviewedAt = &parsed
	}
	if !created.Valid {
		return Errorf(ErrorCodeSkillInvalid, "skill created_at is not valid")
	}
	createdAt, err := parseTime(created.String)
	if err != nil {
		return Errorf(ErrorCodeSkillInvalid, "skill created_at is not valid")
	}
	row.CreatedAt = createdAt
	if !updated.Valid {
		return Errorf(ErrorCodeSkillInvalid, "skill updated_at is not valid")
	}
	updatedAt, err := parseTime(updated.String)
	if err != nil {
		return Errorf(ErrorCodeSkillInvalid, "skill updated_at is not valid")
	}
	row.UpdatedAt = updatedAt
	return nil
}

func (s *sqliteSkillStore) AcceptSkill(ctx context.Context, id, digest, granted string) error {
	if ctx == nil {
		return errNilArgument("ctx")
	}
	if id == "" || digest == "" {
		return Errorf(ErrorCodeInvalidArgument, "skill id and digest must not be empty")
	}
	if !json.Valid([]byte(granted)) {
		return Errorf(ErrorCodeSkillInvalid, "skill grants must be valid JSON")
	}
	now := formatTime(time.Now().UTC())
	result, err := s.db.ExecContext(ctx, `
UPDATE skill_package
SET state = ?, granted_capabilities_json = ?, reviewed_at = ?, updated_at = ?
WHERE id = ? AND state = ? AND content_digest = ?
`, SkillStateActive, granted, now, now, id, SkillStateQuarantined, digest)
	if err != nil {
		return classifyBusy(codedError(ErrorCodeStorageUnavailable, "accept skill", err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return codedError(ErrorCodeStorageUnavailable, "accept skill", err)
	}
	if affected == 1 {
		return nil
	}
	return s.classifyReviewConflict(ctx, id, digest)
}

func (s *sqliteSkillStore) RejectSkill(ctx context.Context, id string) error {
	if ctx == nil {
		return errNilArgument("ctx")
	}
	if id == "" {
		return Errorf(ErrorCodeInvalidArgument, "skill id must not be empty")
	}
	now := formatTime(time.Now().UTC())
	result, err := s.db.ExecContext(ctx, `
UPDATE skill_package
SET state = ?, reviewed_at = ?, updated_at = ?
WHERE id = ? AND state = ?
`, SkillStateRejected, now, now, id, SkillStateQuarantined)
	if err != nil {
		return classifyBusy(codedError(ErrorCodeStorageUnavailable, "reject skill", err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return codedError(ErrorCodeStorageUnavailable, "reject skill", err)
	}
	if affected == 1 {
		return nil
	}
	return s.classifyReviewConflict(ctx, id, "")
}

func (s *sqliteSkillStore) classifyReviewConflict(ctx context.Context, id, digest string) error {
	var state, current string
	err := s.db.QueryRowContext(ctx, `SELECT state, content_digest FROM skill_package WHERE id = ?`, id).Scan(&state, &current)
	if err == sql.ErrNoRows {
		return Errorf(ErrorCodeSkillNotFound, "skill is not registered")
	}
	if err != nil {
		return classifyBusy(codedError(ErrorCodeStorageUnavailable, "read skill", err))
	}
	if digest != "" && current != digest {
		return Errorf(ErrorCodeSkillDigestChanged, "skill content changed since review")
	}
	return Errorf(ErrorCodeSkillInvalid, "skill is not reviewable")
}
