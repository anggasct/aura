package skills

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const (
	defaultSkillListLimit = 50
	maxSkillListLimit     = 1000
)

type SQLiteStore struct {
	db *sql.DB
}

var _ Registry = (*SQLiteStore)(nil)

func NewSQLiteStore(db *sql.DB) (*SQLiteStore, error) {
	if db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	return &SQLiteStore{db: db}, nil
}

func validSkillState(state string) bool {
	switch state {
	case StateQuarantined, StateActive, StateDisabled, StateRejected, StateConflict:
		return true
	default:
		return false
	}
}

func (s *SQLiteStore) UpsertScan(ctx context.Context, record *Record) (bool, error) {
	if s == nil || s.db == nil {
		return false, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if ctx == nil {
		return false, errNilArgument("ctx")
	}
	if record == nil {
		return false, errNilArgument("record")
	}
	if record.ID == "" || record.Name == "" || record.Origin == "" || record.Digest == "" {
		return false, Errorf(ErrorCodeSkillInvalid, "skill identity fields must not be empty")
	}
	if !validSkillState(record.State) {
		return false, Errorf(ErrorCodeSkillInvalid, "skill state is not valid")
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"validation", record.Validation},
		{"requested", record.Requested},
		{"granted", record.Granted},
	} {
		if !json.Valid([]byte(field.value)) {
			return false, Errorf(ErrorCodeSkillInvalid, "skill %s must be valid JSON", field.name)
		}
	}
	var reviewed any
	if record.ReviewedAt != "" {
		parsed, err := sqlite.ParseTime(record.ReviewedAt)
		if err != nil {
			return false, Errorf(ErrorCodeSkillInvalid, "skill reviewed_at is not valid")
		}
		reviewed = sqlite.FormatTime(parsed)
	}
	now := sqlite.FormatTime(time.Now().UTC())
	result, err := sqlite.Conn(ctx, s.db).ExecContext(ctx, `
INSERT INTO skill_package (id, canonical_name, origin_json, content_digest, state, validation_json, requested_capabilities_json, granted_capabilities_json, reviewed_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO NOTHING
`, record.ID, record.Name, record.Origin, record.Digest, record.State, record.Validation, record.Requested, record.Granted, reviewed, now, now)
	if err != nil {
		return false, fmt.Errorf("record skill scan: %w", sqlite.Classify(err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("record skill scan: %w", sqlite.Classify(err))
	}
	return affected == 1, nil
}

func (s *SQLiteStore) Get(ctx context.Context, id string) (Record, error) {
	if s == nil || s.db == nil {
		return Record{}, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if ctx == nil {
		return Record{}, errNilArgument("ctx")
	}
	if id == "" {
		return Record{}, Errorf(ErrorCodeInvalidArgument, "skill id must not be empty")
	}
	var record Record
	var reviewed, created, updated sql.NullString
	err := sqlite.Conn(ctx, s.db).QueryRowContext(ctx, `
SELECT id, canonical_name, origin_json, content_digest, state, validation_json, requested_capabilities_json, granted_capabilities_json, reviewed_at, created_at, updated_at
FROM skill_package WHERE id = ?
`, id).Scan(&record.ID, &record.Name, &record.Origin, &record.Digest, &record.State, &record.Validation, &record.Requested, &record.Granted, &reviewed, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, Errorf(ErrorCodeSkillNotFound, "skill is not registered")
	}
	if err != nil {
		return Record{}, fmt.Errorf("read skill: %w", sqlite.Classify(err))
	}
	if err := bindSkillTimes(&record, reviewed, created, updated); err != nil {
		return Record{}, err
	}
	return record, nil
}

func (s *SQLiteStore) ListByState(ctx context.Context, state string, limit int) ([]Record, error) {
	if s == nil || s.db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
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
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx, `
SELECT id, canonical_name, origin_json, content_digest, state, validation_json, requested_capabilities_json, granted_capabilities_json, reviewed_at, created_at, updated_at
FROM skill_package WHERE state = ? ORDER BY updated_at DESC, id ASC LIMIT ?
`, state, limit)
	if err != nil {
		return nil, fmt.Errorf("list skills: %w", sqlite.Classify(err))
	}
	defer func() { _ = rows.Close() }()
	return scanSkillRecords(rows, limit)
}

func (s *SQLiteStore) ListAll(ctx context.Context, limit int) ([]Record, error) {
	if s == nil || s.db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if ctx == nil {
		return nil, errNilArgument("ctx")
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
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx, `
SELECT id, canonical_name, origin_json, content_digest, state, validation_json, requested_capabilities_json, granted_capabilities_json, reviewed_at, created_at, updated_at
FROM skill_package ORDER BY updated_at DESC, id ASC LIMIT ?
`, limit)
	if err != nil {
		return nil, fmt.Errorf("list skills: %w", sqlite.Classify(err))
	}
	defer func() { _ = rows.Close() }()
	return scanSkillRecords(rows, limit)
}

func scanSkillRecords(rows *sql.Rows, limit int) ([]Record, error) {
	out := make([]Record, 0, limit)
	for rows.Next() {
		var record Record
		var reviewed, created, updated sql.NullString
		if err := rows.Scan(&record.ID, &record.Name, &record.Origin, &record.Digest, &record.State, &record.Validation, &record.Requested, &record.Granted, &reviewed, &created, &updated); err != nil {
			return nil, fmt.Errorf("list skills: %w", sqlite.Classify(err))
		}
		if err := bindSkillTimes(&record, reviewed, created, updated); err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list skills: %w", sqlite.Classify(err))
	}
	return out, nil
}

func bindSkillTimes(record *Record, reviewed, created, updated sql.NullString) error {
	if reviewed.Valid && reviewed.String != "" {
		parsed, err := sqlite.ParseTime(reviewed.String)
		if err != nil {
			return Errorf(ErrorCodeSkillInvalid, "skill reviewed_at is not valid")
		}
		record.ReviewedAt = sqlite.FormatTime(parsed)
	}
	if !created.Valid {
		return Errorf(ErrorCodeSkillInvalid, "skill created_at is not valid")
	}
	if _, err := sqlite.ParseTime(created.String); err != nil {
		return Errorf(ErrorCodeSkillInvalid, "skill created_at is not valid")
	}
	if !updated.Valid {
		return Errorf(ErrorCodeSkillInvalid, "skill updated_at is not valid")
	}
	if _, err := sqlite.ParseTime(updated.String); err != nil {
		return Errorf(ErrorCodeSkillInvalid, "skill updated_at is not valid")
	}
	return nil
}

func (s *SQLiteStore) AcceptSkill(ctx context.Context, id, digest, granted string) error {
	if s == nil || s.db == nil {
		return fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if ctx == nil {
		return errNilArgument("ctx")
	}
	if id == "" || digest == "" {
		return Errorf(ErrorCodeInvalidArgument, "skill id and digest must not be empty")
	}
	if !json.Valid([]byte(granted)) {
		return Errorf(ErrorCodeSkillInvalid, "skill grants must be valid JSON")
	}
	now := sqlite.FormatTime(time.Now().UTC())
	result, err := sqlite.Conn(ctx, s.db).ExecContext(ctx, `
UPDATE skill_package
SET state = ?, granted_capabilities_json = ?, reviewed_at = ?, updated_at = ?
WHERE id = ? AND state = ? AND content_digest = ?
`, StateActive, granted, now, now, id, StateQuarantined, digest)
	if err != nil {
		return fmt.Errorf("accept skill: %w", sqlite.Classify(err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("accept skill: %w", sqlite.Classify(err))
	}
	if affected == 1 {
		return nil
	}
	return s.classifyReviewConflict(ctx, id, digest)
}

func (s *SQLiteStore) RejectSkill(ctx context.Context, id string) error {
	if s == nil || s.db == nil {
		return fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if ctx == nil {
		return errNilArgument("ctx")
	}
	if id == "" {
		return Errorf(ErrorCodeInvalidArgument, "skill id must not be empty")
	}
	now := sqlite.FormatTime(time.Now().UTC())
	result, err := sqlite.Conn(ctx, s.db).ExecContext(ctx, `
UPDATE skill_package
SET state = ?, reviewed_at = ?, updated_at = ?
WHERE id = ? AND state = ?
`, StateRejected, now, now, id, StateQuarantined)
	if err != nil {
		return fmt.Errorf("reject skill: %w", sqlite.Classify(err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("reject skill: %w", sqlite.Classify(err))
	}
	if affected == 1 {
		return nil
	}
	return s.classifyReviewConflict(ctx, id, "")
}

func (s *SQLiteStore) DisableSkill(ctx context.Context, id string) error {
	if s == nil || s.db == nil {
		return fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if ctx == nil {
		return errNilArgument("ctx")
	}
	if id == "" {
		return Errorf(ErrorCodeInvalidArgument, "skill id must not be empty")
	}
	now := sqlite.FormatTime(time.Now().UTC())
	result, err := sqlite.Conn(ctx, s.db).ExecContext(ctx, `
UPDATE skill_package
SET state = ?, updated_at = ?
WHERE id = ? AND state = ?
`, StateDisabled, now, id, StateActive)
	if err != nil {
		return fmt.Errorf("disable skill: %w", sqlite.Classify(err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("disable skill: %w", sqlite.Classify(err))
	}
	if affected == 1 {
		return nil
	}
	return s.classifyReviewConflict(ctx, id, "")
}

func (s *SQLiteStore) classifyReviewConflict(ctx context.Context, id, digest string) error {
	var state, current string
	err := sqlite.Conn(ctx, s.db).QueryRowContext(ctx, `SELECT state, content_digest FROM skill_package WHERE id = ?`, id).Scan(&state, &current)
	if errors.Is(err, sql.ErrNoRows) {
		return Errorf(ErrorCodeSkillNotFound, "skill is not registered")
	}
	if err != nil {
		return fmt.Errorf("read skill: %w", sqlite.Classify(err))
	}
	if digest != "" && current != digest {
		return Errorf(ErrorCodeSkillDigestChanged, "skill content changed since review")
	}
	return Errorf(ErrorCodeSkillInvalid, "skill state does not allow the transition")
}
