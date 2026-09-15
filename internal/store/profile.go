package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type ProfileFact struct {
	ID               string
	OwnerID          string
	Category         string
	Key              string
	Value            string
	ValueDigest      string
	Status           string
	Origin           string
	OwnerVerified    bool
	Confidence       float64
	ConfidencePolicy string
	ConflictsWithID  string
	ExpiresAt        *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type ProfileEvidence struct {
	FactID        string
	SourceEventID string
	SourceDigest  string
	Provider      string
	Model         string
	ModelVersion  string
	PromptVersion string
	PromptDigest  string
	ObservedAt    time.Time
}

type ProfileStore interface {
	InsertFact(ctx context.Context, fact *ProfileFact) error
	GetFact(ctx context.Context, id string) (ProfileFact, bool, error)
	UpdateFact(ctx context.Context, fact *ProfileFact) error
	AddEvidence(ctx context.Context, evidence *ProfileEvidence) (bool, error)
	ListEvidence(ctx context.Context, factID string) ([]ProfileEvidence, error)
	ListActive(ctx context.Context, ownerID, category, key string) ([]ProfileFact, error)
}

type sqliteProfileStore struct {
	db *sql.DB
}

func NewProfileStore(db *sql.DB) ProfileStore {
	return &sqliteProfileStore{db: db}
}

var profileCategories = map[string]bool{
	"preference": true,
	"tool":       true,
	"language":   true,
	"timezone":   true,
	"project":    true,
	"habit":      true,
}

var profileStatuses = map[string]bool{
	"candidate": true,
	"active":    true,
	"rejected":  true,
	"expired":   true,
	"deleted":   true,
}

func validProfileOrigin(origin string) bool {
	return origin == "derived" || origin == "owner"
}

func checkProfileFact(fact *ProfileFact) error {
	if fact == nil {
		return errNilArgument("fact")
	}
	if strings.TrimSpace(fact.ID) == "" || strings.TrimSpace(fact.OwnerID) == "" {
		return Errorf(ErrorCodeInvalidArgument, "profile fact identity must not be empty")
	}
	if !profileCategories[fact.Category] {
		return Errorf(ErrorCodeProfileInvalid, "profile fact category is not valid")
	}
	if strings.TrimSpace(fact.Key) == "" || fact.Value == "" {
		return Errorf(ErrorCodeProfileInvalid, "profile fact key and value must not be empty")
	}
	if strings.TrimSpace(fact.ValueDigest) == "" {
		return Errorf(ErrorCodeProfileInvalid, "profile fact digest must not be empty")
	}
	if !profileStatuses[fact.Status] {
		return Errorf(ErrorCodeProfileInvalid, "profile fact status is not valid")
	}
	if !validProfileOrigin(fact.Origin) {
		return Errorf(ErrorCodeProfileInvalid, "profile fact origin is not valid")
	}
	if fact.Confidence < 0 || fact.Confidence > 1 {
		return Errorf(ErrorCodeProfileInvalid, "profile fact confidence is out of range")
	}
	if strings.TrimSpace(fact.ConfidencePolicy) == "" {
		return Errorf(ErrorCodeProfileInvalid, "profile confidence policy must not be empty")
	}
	if fact.CreatedAt.IsZero() || fact.UpdatedAt.IsZero() {
		return Errorf(ErrorCodeInvalidArgument, "profile fact timestamps must not be zero")
	}
	return nil
}

func profileBool(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func nullProfileText(value string) sql.NullString {
	if value == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: value, Valid: true}
}

func nullProfileTime(value *time.Time) sql.NullString {
	if value == nil || value.IsZero() {
		return sql.NullString{}
	}
	return sql.NullString{String: formatTime(value.UTC()), Valid: true}
}

func (s *sqliteProfileStore) InsertFact(ctx context.Context, fact *ProfileFact) error {
	if s.db == nil {
		return errNilArgument("db")
	}
	if err := checkProfileFact(fact); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO profile_fact (
			id, owner_id, category, fact_key, fact_value, value_digest, status, origin,
			owner_verified, confidence, confidence_policy_version, conflicts_with_id,
			expires_at, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		fact.ID, fact.OwnerID, fact.Category, fact.Key, fact.Value, fact.ValueDigest,
		fact.Status, fact.Origin, profileBool(fact.OwnerVerified), fact.Confidence,
		fact.ConfidencePolicy, nullProfileText(fact.ConflictsWithID),
		nullProfileTime(fact.ExpiresAt), formatTime(fact.CreatedAt.UTC()), formatTime(fact.UpdatedAt.UTC()),
	)
	if err != nil {
		if isConstraintUnique(err) {
			return Errorf(ErrorCodeProfileConflict, "profile fact already exists")
		}
		return classifyBusy(fmt.Errorf("insert profile fact: %w", err))
	}
	return nil
}

func scanProfileFact(rows *sql.Rows) (ProfileFact, error) {
	var fact ProfileFact
	var verified int64
	var conflicts sql.NullString
	var expires sql.NullString
	var created, updated string
	if err := rows.Scan(
		&fact.ID, &fact.OwnerID, &fact.Category, &fact.Key, &fact.Value, &fact.ValueDigest,
		&fact.Status, &fact.Origin, &verified, &fact.Confidence, &fact.ConfidencePolicy,
		&conflicts, &expires, &created, &updated,
	); err != nil {
		return ProfileFact{}, fmt.Errorf("scan profile fact: %w", err)
	}
	fact.OwnerVerified = verified == 1
	fact.ConflictsWithID = conflicts.String
	createdAt, err := parseTime(created)
	if err != nil {
		return ProfileFact{}, fmt.Errorf("parse profile fact timestamps: %w", err)
	}
	updatedAt, err := parseTime(updated)
	if err != nil {
		return ProfileFact{}, fmt.Errorf("parse profile fact timestamps: %w", err)
	}
	fact.CreatedAt, fact.UpdatedAt = createdAt, updatedAt
	if expires.Valid {
		expiresAt, err := parseTime(expires.String)
		if err != nil {
			return ProfileFact{}, fmt.Errorf("parse profile fact expiry: %w", err)
		}
		fact.ExpiresAt = &expiresAt
	}
	return fact, nil
}

const selectProfileFactColumns = `
	id, owner_id, category, fact_key, fact_value, value_digest, status, origin,
	owner_verified, confidence, confidence_policy_version, conflicts_with_id,
	expires_at, created_at, updated_at`

func (s *sqliteProfileStore) GetFact(ctx context.Context, id string) (ProfileFact, bool, error) {
	if s.db == nil {
		return ProfileFact{}, false, errNilArgument("db")
	}
	if strings.TrimSpace(id) == "" {
		return ProfileFact{}, false, Errorf(ErrorCodeInvalidArgument, "profile fact id must not be empty")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+selectProfileFactColumns+` FROM profile_fact WHERE id = ?`, id,
	)
	if err != nil {
		return ProfileFact{}, false, classifyBusy(fmt.Errorf("get profile fact: %w", err))
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return ProfileFact{}, false, classifyBusy(fmt.Errorf("get profile fact: %w", err))
		}
		return ProfileFact{}, false, nil
	}
	fact, err := scanProfileFact(rows)
	if err != nil {
		return ProfileFact{}, false, err
	}
	if err := rows.Err(); err != nil {
		return ProfileFact{}, false, classifyBusy(fmt.Errorf("get profile fact: %w", err))
	}
	return fact, true, nil
}

func (s *sqliteProfileStore) UpdateFact(ctx context.Context, fact *ProfileFact) error {
	if s.db == nil {
		return errNilArgument("db")
	}
	if err := checkProfileFact(fact); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE profile_fact SET
			fact_value = ?, value_digest = ?, status = ?, origin = ?,
			owner_verified = ?, confidence = ?, confidence_policy_version = ?,
			conflicts_with_id = ?, expires_at = ?, updated_at = ?
		WHERE id = ?`,
		fact.Value, fact.ValueDigest, fact.Status, fact.Origin,
		profileBool(fact.OwnerVerified), fact.Confidence, fact.ConfidencePolicy,
		nullProfileText(fact.ConflictsWithID), nullProfileTime(fact.ExpiresAt),
		formatTime(fact.UpdatedAt.UTC()), fact.ID,
	)
	if err != nil {
		if isConstraintUnique(err) {
			return Errorf(ErrorCodeProfileConflict, "profile fact update conflicts")
		}
		return classifyBusy(fmt.Errorf("update profile fact: %w", err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return classifyBusy(fmt.Errorf("update profile fact: %w", err))
	}
	if affected == 0 {
		return Errorf(ErrorCodeProfileNotFound, "profile fact does not exist")
	}
	return nil
}

func (s *sqliteProfileStore) AddEvidence(ctx context.Context, evidence *ProfileEvidence) (bool, error) {
	if s.db == nil {
		return false, errNilArgument("db")
	}
	if evidence == nil {
		return false, errNilArgument("evidence")
	}
	if strings.TrimSpace(evidence.FactID) == "" || strings.TrimSpace(evidence.SourceEventID) == "" || strings.TrimSpace(evidence.SourceDigest) == "" {
		return false, Errorf(ErrorCodeInvalidArgument, "profile evidence identity must not be empty")
	}
	if evidence.ObservedAt.IsZero() {
		return false, Errorf(ErrorCodeInvalidArgument, "profile evidence timestamp must not be zero")
	}
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO profile_evidence (
			fact_id, source_event_id, source_digest, extractor_provider, extractor_model,
			extractor_model_version, prompt_version, prompt_digest, observed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(fact_id, source_event_id, source_digest) DO NOTHING`,
		evidence.FactID, evidence.SourceEventID, evidence.SourceDigest,
		nullProfileText(evidence.Provider), nullProfileText(evidence.Model),
		nullProfileText(evidence.ModelVersion), nullProfileText(evidence.PromptVersion),
		nullProfileText(evidence.PromptDigest), formatTime(evidence.ObservedAt.UTC()),
	)
	if err != nil {
		if isConstraintForeignKey(err) {
			return false, Errorf(ErrorCodeProfileInvalid, "profile evidence references unknown rows")
		}
		return false, classifyBusy(fmt.Errorf("add profile evidence: %w", err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, classifyBusy(fmt.Errorf("add profile evidence: %w", err))
	}
	return affected > 0, nil
}

func (s *sqliteProfileStore) ListEvidence(ctx context.Context, factID string) ([]ProfileEvidence, error) {
	if s.db == nil {
		return nil, errNilArgument("db")
	}
	if strings.TrimSpace(factID) == "" {
		return nil, Errorf(ErrorCodeInvalidArgument, "profile fact id must not be empty")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT fact_id, source_event_id, source_digest, extractor_provider, extractor_model,
			extractor_model_version, prompt_version, prompt_digest, observed_at
		FROM profile_evidence WHERE fact_id = ? ORDER BY observed_at ASC, source_event_id ASC, source_digest ASC`,
		factID,
	)
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("list profile evidence: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var out []ProfileEvidence
	for rows.Next() {
		var evidence ProfileEvidence
		var provider, model, modelVersion, promptVersion, promptDigest sql.NullString
		var observed string
		if err := rows.Scan(
			&evidence.FactID, &evidence.SourceEventID, &evidence.SourceDigest,
			&provider, &model, &modelVersion, &promptVersion, &promptDigest, &observed,
		); err != nil {
			return nil, fmt.Errorf("scan profile evidence: %w", err)
		}
		evidence.Provider, evidence.Model = provider.String, model.String
		evidence.ModelVersion, evidence.PromptVersion = modelVersion.String, promptVersion.String
		evidence.PromptDigest = promptDigest.String
		observedAt, err := parseTime(observed)
		if err != nil {
			return nil, fmt.Errorf("parse profile evidence timestamp: %w", err)
		}
		evidence.ObservedAt = observedAt
		out = append(out, evidence)
	}
	if err := rows.Err(); err != nil {
		return nil, classifyBusy(fmt.Errorf("list profile evidence: %w", err))
	}
	return out, nil
}

func (s *sqliteProfileStore) ListActive(ctx context.Context, ownerID, category, key string) ([]ProfileFact, error) {
	if s.db == nil {
		return nil, errNilArgument("db")
	}
	if strings.TrimSpace(ownerID) == "" || strings.TrimSpace(category) == "" || strings.TrimSpace(key) == "" {
		return nil, Errorf(ErrorCodeInvalidArgument, "profile lookup scope must not be empty")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+selectProfileFactColumns+`
		FROM profile_fact
		WHERE owner_id = ? AND category = ? AND fact_key = ? AND status = 'active'
		ORDER BY id ASC`,
		ownerID, category, key,
	)
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("list active profile facts: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var out []ProfileFact
	for rows.Next() {
		fact, err := scanProfileFact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, fact)
	}
	if err := rows.Err(); err != nil {
		return nil, classifyBusy(fmt.Errorf("list active profile facts: %w", err))
	}
	return out, nil
}
