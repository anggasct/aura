package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/anggasct/aura/internal/profile"
	"github.com/anggasct/aura/internal/store"
)

const profileOwnerSessionPrefix = "profile-"

type profileRegistry struct {
	db *sql.DB
}

func newProfileRegistry(db *sql.DB) *profileRegistry {
	return &profileRegistry{db: db}
}

func profileFactFromStore(row *store.ProfileFact) profile.Fact {
	fact := profile.Fact{
		ID: row.ID, OwnerID: row.OwnerID, Category: row.Category, Key: row.Key,
		Value: row.Value, ValueDigest: row.ValueDigest, Status: row.Status,
		Origin: row.Origin, OwnerVerified: row.OwnerVerified, Confidence: row.Confidence,
		ConfidencePolicy: row.ConfidencePolicy, ConflictsWithID: row.ConflictsWithID,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
	if row.ExpiresAt != nil {
		expires := *row.ExpiresAt
		fact.ExpiresAt = &expires
	}
	return fact
}

func profileFactToStore(fact *profile.Fact) *store.ProfileFact {
	row := &store.ProfileFact{
		ID: fact.ID, OwnerID: fact.OwnerID, Category: fact.Category, Key: fact.Key,
		Value: fact.Value, ValueDigest: fact.ValueDigest, Status: fact.Status,
		Origin: fact.Origin, OwnerVerified: fact.OwnerVerified, Confidence: fact.Confidence,
		ConfidencePolicy: fact.ConfidencePolicy, ConflictsWithID: fact.ConflictsWithID,
		CreatedAt: fact.CreatedAt, UpdatedAt: fact.UpdatedAt,
	}
	if fact.ExpiresAt != nil {
		expires := *fact.ExpiresAt
		row.ExpiresAt = &expires
	}
	return row
}

func (r *profileRegistry) InsertCandidate(ctx context.Context, fact *profile.Fact, evidence *profile.Evidence, minConfidence float64) (profile.Fact, error) {
	if fact == nil {
		return profile.Fact{}, errors.New("cli: profile fact must not be nil")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return profile.Fact{}, fmt.Errorf("cli: begin profile insert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	stored := *fact
	if minConfidence > 0 {
		var conflicts sql.NullString
		if err := tx.QueryRowContext(ctx,
			`SELECT id FROM profile_fact
			 WHERE owner_id = ? AND category = ? AND fact_key = ? AND status = 'active' AND value_digest != ? AND id != ?
			 ORDER BY id ASC LIMIT 1`,
			fact.OwnerID, fact.Category, fact.Key, fact.ValueDigest, fact.ID,
		).Scan(&conflicts); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return profile.Fact{}, fmt.Errorf("cli: read conflicting active fact: %w", err)
		} else if conflicts.Valid {
			stored.ConflictsWithID = conflicts.String
		}
	}
	if stored.ConflictsWithID == "" && stored.Confidence >= minConfidence && minConfidence > 0 {
		stored.Status = profile.StatusActive
	}
	if err := insertProfileFactTx(ctx, tx, profileFactToStore(&stored)); err != nil {
		if code, ok := store.CodeOf(err); ok && code == store.ErrorCodeProfileConflict {
			return profile.Fact{}, profile.Errorf(profile.ErrorCodeProfileConflict, "profile fact already exists")
		}
		return profile.Fact{}, err
	}
	if evidence != nil {
		if err := insertProfileEvidenceTx(ctx, tx, stored.ID, evidence); err != nil {
			return profile.Fact{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return profile.Fact{}, fmt.Errorf("cli: commit profile insert: %w", err)
	}
	return stored, nil
}

func (r *profileRegistry) Reinforce(ctx context.Context, factID string, evidence *profile.Evidence, minConfidence float64) (profile.Fact, error) {
	if evidence == nil {
		return profile.Fact{}, errors.New("cli: profile evidence must not be nil")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return profile.Fact{}, fmt.Errorf("cli: begin profile reinforce: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	row, found, err := getProfileFactTx(ctx, tx, factID)
	if err != nil {
		return profile.Fact{}, err
	}
	if !found {
		return profile.Fact{}, profile.Errorf(profile.ErrorCodeProfileNotFound, "fact does not exist")
	}
	fact := profileFactFromStore(&row)
	if fact.Status != profile.StatusCandidate && fact.Status != profile.StatusActive {
		return profile.Fact{}, profile.Errorf(profile.ErrorCodeProfileConflict, "fact is not reinforceable")
	}
	if err := insertProfileEvidenceTx(ctx, tx, fact.ID, evidence); err != nil {
		return profile.Fact{}, err
	}
	var count int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM profile_evidence WHERE fact_id = ?`, factID,
	).Scan(&count); err != nil {
		return profile.Fact{}, fmt.Errorf("cli: count profile evidence: %w", err)
	}
	fact.Confidence = profile.ConfidenceV1(count)
	if fact.Status == profile.StatusCandidate && fact.Confidence >= minConfidence {
		var conflicts sql.NullString
		if err := tx.QueryRowContext(ctx,
			`SELECT id FROM profile_fact
			 WHERE owner_id = ? AND category = ? AND fact_key = ? AND status = 'active' AND value_digest != ? AND id != ?
			 ORDER BY id ASC LIMIT 1`,
			fact.OwnerID, fact.Category, fact.Key, fact.ValueDigest, fact.ID,
		).Scan(&conflicts); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return profile.Fact{}, fmt.Errorf("cli: read conflicting active fact: %w", err)
		} else if conflicts.Valid {
			fact.ConflictsWithID = conflicts.String
		} else {
			fact.Status = profile.StatusActive
		}
	}
	if err := updateProfileFactTx(ctx, tx, profileFactToStore(&fact)); err != nil {
		return profile.Fact{}, err
	}
	if err := tx.Commit(); err != nil {
		return profile.Fact{}, fmt.Errorf("cli: commit profile reinforce: %w", err)
	}
	return fact, nil
}

func (r *profileRegistry) SetState(ctx context.Context, factID, status, conflictsWith string, confidence float64, verified bool, at time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("cli: begin profile state change: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	row, found, err := getProfileFactTx(ctx, tx, factID)
	if err != nil {
		return err
	}
	if !found {
		return profile.Errorf(profile.ErrorCodeProfileNotFound, "fact does not exist")
	}
	fact := profileFactFromStore(&row)
	fact.Status = status
	fact.ConflictsWithID = conflictsWith
	fact.Confidence = confidence
	fact.OwnerVerified = verified
	fact.UpdatedAt = at
	if err := updateProfileFactTx(ctx, tx, profileFactToStore(&fact)); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *profileRegistry) ReviveCandidate(ctx context.Context, factID string, at time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("cli: begin profile revive: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	row, found, err := getProfileFactTx(ctx, tx, factID)
	if err != nil {
		return err
	}
	if !found {
		return profile.Errorf(profile.ErrorCodeProfileNotFound, "fact does not exist")
	}
	fact := profileFactFromStore(&row)
	fact.Status = profile.StatusCandidate
	fact.UpdatedAt = at
	if err := updateProfileFactTx(ctx, tx, profileFactToStore(&fact)); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *profileRegistry) Get(ctx context.Context, id string) (profile.Fact, bool, error) {
	row, found, err := store.NewProfileStore(r.db).GetFact(ctx, id)
	if err != nil || !found {
		return profile.Fact{}, false, err
	}
	return profileFactFromStore(&row), true, nil
}

func (r *profileRegistry) Active(ctx context.Context, ownerID, category, key string) ([]profile.Fact, error) {
	rows, err := store.NewProfileStore(r.db).ListActive(ctx, ownerID, category, key)
	if err != nil {
		return nil, err
	}
	out := make([]profile.Fact, 0, len(rows))
	for i := range rows {
		out = append(out, profileFactFromStore(&rows[i]))
	}
	return out, nil
}

func (r *profileRegistry) Evidence(ctx context.Context, factID string) ([]profile.Evidence, error) {
	rows, err := store.NewProfileStore(r.db).ListEvidence(ctx, factID)
	if err != nil {
		return nil, err
	}
	out := make([]profile.Evidence, 0, len(rows))
	for i := range rows {
		out = append(out, profile.Evidence{
			SourceEventID: rows[i].SourceEventID, SourceDigest: rows[i].SourceDigest,
			Provider: rows[i].Provider, Model: rows[i].Model, ModelVersion: rows[i].ModelVersion,
			PromptVersion: rows[i].PromptVersion, PromptDigest: rows[i].PromptDigest,
			ObservedAt: rows[i].ObservedAt,
		})
	}
	return out, nil
}

func (r *profileRegistry) Expire(ctx context.Context, now time.Time) (int, error) {
	result, err := r.db.ExecContext(ctx,
		`UPDATE profile_fact SET status = 'expired', updated_at = ?
		 WHERE status IN ('candidate','active') AND expires_at IS NOT NULL AND expires_at <= ?`,
		now.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return 0, fmt.Errorf("cli: expire profile facts: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("cli: expire profile facts: %w", err)
	}
	return int(affected), nil
}

func (r *profileRegistry) Search(ctx context.Context, ownerID, category, query string, limit int, now time.Time) ([]profile.SearchHit, error) {
	rows, err := store.NewProfileStore(r.db).SearchFacts(ctx, ownerID, category, query, limit, now)
	if err != nil {
		return nil, err
	}
	out := make([]profile.SearchHit, 0, len(rows))
	for i := range rows {
		out = append(out, profile.SearchHit{Fact: profileFactFromStore(&rows[i])})
	}
	return out, nil
}

func (r *profileRegistry) List(ctx context.Context, ownerID, status, category string, limit int) ([]profile.Fact, error) {
	rows, err := store.NewProfileStore(r.db).ListFacts(ctx, ownerID, status, category, limit)
	if err != nil {
		return nil, err
	}
	out := make([]profile.Fact, 0, len(rows))
	for i := range rows {
		out = append(out, profileFactFromStore(&rows[i]))
	}
	return out, nil
}

func insertProfileFactTx(ctx context.Context, tx *sql.Tx, fact *store.ProfileFact) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO profile_fact (
			id, owner_id, category, fact_key, fact_value, value_digest, status, origin,
			owner_verified, confidence, confidence_policy_version, conflicts_with_id,
			expires_at, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		fact.ID, fact.OwnerID, fact.Category, fact.Key, fact.Value, fact.ValueDigest,
		fact.Status, fact.Origin, store.ProfileVerifiedFlag(fact.OwnerVerified), fact.Confidence,
		fact.ConfidencePolicy, store.ProfileConflictsWith(fact.ConflictsWithID), store.ProfileExpiresAt(fact.ExpiresAt),
		fact.CreatedAt.UTC().Format(time.RFC3339Nano), fact.UpdatedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("cli: insert profile fact: %w", err)
	}
	return nil
}

func insertProfileEvidenceTx(ctx context.Context, tx *sql.Tx, factID string, evidence *profile.Evidence) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO profile_evidence (
			fact_id, source_event_id, source_digest, extractor_provider, extractor_model,
			extractor_model_version, prompt_version, prompt_digest, observed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(fact_id, source_event_id, source_digest) DO NOTHING`,
		factID, evidence.SourceEventID, evidence.SourceDigest,
		evidence.Provider, evidence.Model, evidence.ModelVersion, evidence.PromptVersion,
		evidence.PromptDigest, evidence.ObservedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("cli: insert profile evidence: %w", err)
	}
	return nil
}

func getProfileFactTx(ctx context.Context, tx *sql.Tx, id string) (store.ProfileFact, bool, error) {
	var row store.ProfileFact
	var verified int64
	var conflicts, expires sql.NullString
	var created, updated string
	err := tx.QueryRowContext(ctx,
		`SELECT id, owner_id, category, fact_key, fact_value, value_digest, status, origin,
			owner_verified, confidence, confidence_policy_version, conflicts_with_id,
			expires_at, created_at, updated_at
		FROM profile_fact WHERE id = ?`, id,
	).Scan(&row.ID, &row.OwnerID, &row.Category, &row.Key, &row.Value, &row.ValueDigest,
		&row.Status, &row.Origin, &verified, &row.Confidence, &row.ConfidencePolicy,
		&conflicts, &expires, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ProfileFact{}, false, nil
	}
	if err != nil {
		return store.ProfileFact{}, false, fmt.Errorf("cli: read profile fact: %w", err)
	}
	row.OwnerVerified = verified == 1
	row.ConflictsWithID = conflicts.String
	createdAt, err := time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return store.ProfileFact{}, false, fmt.Errorf("cli: parse profile fact timestamps: %w", err)
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, updated)
	if err != nil {
		return store.ProfileFact{}, false, fmt.Errorf("cli: parse profile fact timestamps: %w", err)
	}
	row.CreatedAt, row.UpdatedAt = createdAt, updatedAt
	if expires.Valid && expires.String != "" {
		expiresAt, err := time.Parse(time.RFC3339Nano, expires.String)
		if err != nil {
			return store.ProfileFact{}, false, fmt.Errorf("cli: parse profile fact expiry: %w", err)
		}
		row.ExpiresAt = &expiresAt
	}
	return row, true, nil
}

func updateProfileFactTx(ctx context.Context, tx *sql.Tx, fact *store.ProfileFact) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE profile_fact SET
			status = ?, confidence = ?, owner_verified = ?, conflicts_with_id = ?,
			value_digest = ?, fact_value = ?, expires_at = ?, updated_at = ?
		WHERE id = ?`,
		fact.Status, fact.Confidence, store.ProfileVerifiedFlag(fact.OwnerVerified),
		store.ProfileConflictsWith(fact.ConflictsWithID), fact.ValueDigest, fact.Value, store.ProfileExpiresAt(fact.ExpiresAt),
		fact.UpdatedAt.UTC().Format(time.RFC3339Nano), fact.ID,
	)
	if err != nil {
		return fmt.Errorf("cli: update profile fact: %w", err)
	}
	return nil
}

type profileActionSink struct {
	db    *sql.DB
	owner string
}

func newProfileActionSink(db *sql.DB, owner string) *profileActionSink {
	return &profileActionSink{db: db, owner: owner}
}

func (s *profileActionSink) RecordOwnerAction(ctx context.Context, action *profile.OwnerAction) error {
	if action == nil {
		return errors.New("cli: owner action must not be nil")
	}
	sessionID := profileOwnerSessionPrefix + action.OwnerID
	sessions := store.NewSessionService(s.db)
	if _, err := sessions.Get(ctx, sessionID); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		now := time.Now().UTC()
		if err := sessions.Create(ctx, &store.Session{
			ID: sessionID, OwnerID: action.OwnerID, CreatedAt: now, UpdatedAt: now, Metadata: json.RawMessage(`{}`),
		}); err != nil {
			if code, ok := store.CodeOf(err); !ok || code != store.ErrorCodeSessionIDConflict {
				return err
			}
		}
	}
	payload, err := json.Marshal(struct {
		Action   string `json:"action"`
		FactID   string `json:"fact_id"`
		Category string `json:"category"`
		Key      string `json:"key"`
		At       string `json:"at"`
	}{
		Action: action.Action, FactID: action.FactID, Category: action.Category,
		Key: action.Key, At: action.At.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return fmt.Errorf("cli: marshal owner action: %w", err)
	}
	events := store.NewEventStore(s.db)
	_, _, err = events.UpsertEvent(ctx, &store.RuntimeEvent{
		ID:            "profile-owner-action-" + action.Action + "-" + action.FactID + "-" + action.At.UTC().Format(time.RFC3339Nano),
		SessionID:     sessionID,
		TurnID:        "owner-action",
		InvocationID:  "owner-action",
		Author:        action.OwnerID,
		Kind:          "profile.owner_action.v1",
		SchemaVersion: 1,
		Payload:       payload,
		CreatedAt:     action.At.UTC(),
	})
	return err
}
