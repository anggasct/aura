package profile

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/anggasct/aura/internal/platform/embedding"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/textsearch"
)

type FactCount struct {
	Status   string
	Category string
	Count    int64
}

type SQLiteStore struct {
	db        *sql.DB
	embedding *EmbeddingOptions
	logger    *slog.Logger
	backfill  embedding.BackfillCursor
}

func NewSQLiteStore(db *sql.DB) (*SQLiteStore, error) {
	if db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	return &SQLiteStore{db: db, logger: slog.Default()}, nil
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
	StatusCandidate: true,
	StatusActive:    true,
	StatusRejected:  true,
	StatusExpired:   true,
	StatusDeleted:   true,
}

func validProfileOrigin(origin string) bool {
	return origin == OriginDerived || origin == OriginOwner || origin == OriginAgent
}

func checkStoredFact(fact *Fact) error {
	if fact == nil {
		return fault.New(ErrorCodeInvalidArgument, "fact must not be nil")
	}
	if strings.TrimSpace(fact.ID) == "" || strings.TrimSpace(fact.OwnerID) == "" {
		return fault.New(ErrorCodeInvalidArgument, "profile fact identity must not be empty")
	}
	if !profileCategories[fact.Category] {
		return fault.New(ErrorCodeProfileInvalid, "profile fact category is not valid")
	}
	if strings.TrimSpace(fact.Key) == "" || fact.Value == "" {
		return fault.New(ErrorCodeProfileInvalid, "profile fact key and value must not be empty")
	}
	if strings.TrimSpace(fact.ValueDigest) == "" {
		return fault.New(ErrorCodeProfileInvalid, "profile fact digest must not be empty")
	}
	if !profileStatuses[fact.Status] {
		return fault.New(ErrorCodeProfileInvalid, "profile fact status is not valid")
	}
	if !validProfileOrigin(fact.Origin) {
		return fault.New(ErrorCodeProfileInvalid, "profile fact origin is not valid")
	}
	if fact.Confidence < 0 || fact.Confidence > 1 {
		return fault.New(ErrorCodeProfileInvalid, "profile fact confidence is out of range")
	}
	if strings.TrimSpace(fact.ConfidencePolicy) == "" {
		return fault.New(ErrorCodeProfileInvalid, "profile confidence policy must not be empty")
	}
	if fact.CreatedAt.IsZero() || fact.UpdatedAt.IsZero() {
		return fault.New(ErrorCodeInvalidArgument, "profile fact timestamps must not be zero")
	}
	return nil
}

func isUniqueConstraint(err error) bool {
	classified := sqlite.Classify(err)
	return fault.HasCode(classified, sqlite.ErrorCodeConstraintUnique) ||
		fault.HasCode(classified, sqlite.ErrorCodeConstraintPrimaryKey)
}

func isForeignKeyConstraint(err error) bool {
	return fault.HasCode(sqlite.Classify(err), sqlite.ErrorCodeConstraintForeignKey)
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
	return sql.NullString{String: sqlite.FormatTime(value.UTC()), Valid: true}
}

const (
	maxProfileQueryRunes       = 500
	minProfileSearchCandidates = 40
	maxProfileSearchCandidates = 200
)

func tokenizeProfileQuery(raw string) ([]string, error) {
	if utf8.RuneCountInString(raw) > maxProfileQueryRunes {
		return nil, fault.New(ErrorCodeInvalidArgument, "profile search query exceeds length bounds")
	}
	tokens := textsearch.Tokenize(raw)
	if len(tokens) == 0 {
		return nil, fault.New(ErrorCodeInvalidArgument, "profile search carries no searchable terms")
	}
	return tokens, nil
}

const selectProfileFactColumns = `
	id, owner_id, category, fact_key, fact_value, value_digest, status, origin,
	owner_verified, confidence, confidence_policy_version, conflicts_with_id,
	expires_at, created_at, updated_at, taint_source`

func scanProfileFact(rows *sql.Rows) (Fact, error) {
	var fact Fact
	var verified int64
	var conflicts sql.NullString
	var expires sql.NullString
	var taintSource sql.NullString
	var created, updated string
	if err := rows.Scan(
		&fact.ID, &fact.OwnerID, &fact.Category, &fact.Key, &fact.Value, &fact.ValueDigest,
		&fact.Status, &fact.Origin, &verified, &fact.Confidence, &fact.ConfidencePolicy,
		&conflicts, &expires, &created, &updated, &taintSource,
	); err != nil {
		return Fact{}, sqlite.Classify(fmt.Errorf("scan profile fact: %w", err))
	}
	fact.OwnerVerified = verified == 1
	fact.ConflictsWithID = conflicts.String
	fact.TaintSource = taintSource.String
	createdAt, err := sqlite.ParseTime(created)
	if err != nil {
		return Fact{}, fmt.Errorf("parse profile fact timestamps: %w", err)
	}
	updatedAt, err := sqlite.ParseTime(updated)
	if err != nil {
		return Fact{}, fmt.Errorf("parse profile fact timestamps: %w", err)
	}
	fact.CreatedAt, fact.UpdatedAt = createdAt, updatedAt
	if expires.Valid {
		expiresAt, err := sqlite.ParseTime(expires.String)
		if err != nil {
			return Fact{}, fmt.Errorf("parse profile fact expiry: %w", err)
		}
		fact.ExpiresAt = &expiresAt
	}
	return fact, nil
}

func (s *SQLiteStore) InsertFact(ctx context.Context, fact *Fact) error {
	if s == nil || s.db == nil {
		return fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if err := checkStoredFact(fact); err != nil {
		return err
	}
	conn := sqlite.Conn(ctx, s.db)
	_, err := conn.ExecContext(ctx,
		`INSERT INTO profile_fact (
			id, owner_id, category, fact_key, fact_value, value_digest, status, origin,
			owner_verified, confidence, confidence_policy_version, conflicts_with_id,
			expires_at, created_at, updated_at, taint_source
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		fact.ID, fact.OwnerID, fact.Category, fact.Key, fact.Value, fact.ValueDigest,
		fact.Status, fact.Origin, profileBool(fact.OwnerVerified), fact.Confidence,
		fact.ConfidencePolicy, nullProfileText(fact.ConflictsWithID),
		nullProfileTime(fact.ExpiresAt), sqlite.FormatTime(fact.CreatedAt.UTC()), sqlite.FormatTime(fact.UpdatedAt.UTC()),
		nullProfileText(fact.TaintSource),
	)
	if err != nil {
		if isUniqueConstraint(err) {
			return fault.New(ErrorCodeProfileConflict, "profile fact already exists")
		}
		return sqlite.Classify(fmt.Errorf("insert profile fact: %w", err))
	}
	return nil
}

func (s *SQLiteStore) GetFact(ctx context.Context, id string) (Fact, bool, error) {
	if s == nil || s.db == nil {
		return Fact{}, false, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if strings.TrimSpace(id) == "" {
		return Fact{}, false, fault.New(ErrorCodeInvalidArgument, "profile fact id must not be empty")
	}
	conn := sqlite.Conn(ctx, s.db)
	rows, err := conn.QueryContext(ctx,
		`SELECT `+selectProfileFactColumns+` FROM profile_fact WHERE id = ?`, id,
	)
	if err != nil {
		return Fact{}, false, sqlite.Classify(fmt.Errorf("get profile fact: %w", err))
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Fact{}, false, sqlite.Classify(fmt.Errorf("get profile fact: %w", err))
		}
		return Fact{}, false, nil
	}
	fact, err := scanProfileFact(rows)
	if err != nil {
		return Fact{}, false, err
	}
	if err := rows.Err(); err != nil {
		return Fact{}, false, sqlite.Classify(fmt.Errorf("get profile fact: %w", err))
	}
	return fact, true, nil
}

func (s *SQLiteStore) UpdateFact(ctx context.Context, fact *Fact) error {
	if s == nil || s.db == nil {
		return fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if err := checkStoredFact(fact); err != nil {
		return err
	}
	conn := sqlite.Conn(ctx, s.db)
	result, err := conn.ExecContext(ctx,
		`UPDATE profile_fact SET
			fact_value = ?, value_digest = ?, status = ?, origin = ?,
			owner_verified = ?, confidence = ?, confidence_policy_version = ?,
			conflicts_with_id = ?, expires_at = ?, updated_at = ?, taint_source = ?
		WHERE id = ?`,
		fact.Value, fact.ValueDigest, fact.Status, fact.Origin,
		profileBool(fact.OwnerVerified), fact.Confidence, fact.ConfidencePolicy,
		nullProfileText(fact.ConflictsWithID), nullProfileTime(fact.ExpiresAt),
		sqlite.FormatTime(fact.UpdatedAt.UTC()), nullProfileText(fact.TaintSource), fact.ID,
	)
	if err != nil {
		if isUniqueConstraint(err) {
			return fault.New(ErrorCodeProfileConflict, "profile fact update conflicts")
		}
		return sqlite.Classify(fmt.Errorf("update profile fact: %w", err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return sqlite.Classify(fmt.Errorf("update profile fact: %w", err))
	}
	if affected == 0 {
		return fault.New(ErrorCodeProfileNotFound, "profile fact does not exist")
	}
	return nil
}

func (s *SQLiteStore) AddEvidence(ctx context.Context, factID string, evidence *Evidence) (bool, error) {
	if s == nil || s.db == nil {
		return false, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if evidence == nil {
		return false, fault.New(ErrorCodeInvalidArgument, "evidence must not be nil")
	}
	if strings.TrimSpace(factID) == "" || strings.TrimSpace(evidence.SourceEventID) == "" || strings.TrimSpace(evidence.SourceDigest) == "" {
		return false, fault.New(ErrorCodeInvalidArgument, "profile evidence identity must not be empty")
	}
	if evidence.ObservedAt.IsZero() {
		return false, fault.New(ErrorCodeInvalidArgument, "profile evidence timestamp must not be zero")
	}
	conn := sqlite.Conn(ctx, s.db)
	result, err := conn.ExecContext(ctx,
		`INSERT INTO profile_evidence (
			fact_id, source_event_id, source_digest, extractor_provider, extractor_model,
			extractor_model_version, prompt_version, prompt_digest, observed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(fact_id, source_event_id, source_digest) DO NOTHING`,
		factID, evidence.SourceEventID, evidence.SourceDigest,
		nullProfileText(evidence.Provider), nullProfileText(evidence.Model),
		nullProfileText(evidence.ModelVersion), nullProfileText(evidence.PromptVersion),
		nullProfileText(evidence.PromptDigest), sqlite.FormatTime(evidence.ObservedAt.UTC()),
	)
	if err != nil {
		if isForeignKeyConstraint(err) {
			return false, fault.New(ErrorCodeProfileInvalid, "profile evidence references unknown rows")
		}
		return false, sqlite.Classify(fmt.Errorf("add profile evidence: %w", err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, sqlite.Classify(fmt.Errorf("add profile evidence: %w", err))
	}
	return affected > 0, nil
}

func (s *SQLiteStore) ListEvidence(ctx context.Context, factID string) ([]Evidence, error) {
	if s == nil || s.db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if strings.TrimSpace(factID) == "" {
		return nil, fault.New(ErrorCodeInvalidArgument, "profile fact id must not be empty")
	}
	conn := sqlite.Conn(ctx, s.db)
	rows, err := conn.QueryContext(ctx,
		`SELECT source_event_id, source_digest, extractor_provider, extractor_model,
			extractor_model_version, prompt_version, prompt_digest, observed_at
		FROM profile_evidence WHERE fact_id = ? ORDER BY observed_at ASC, source_event_id ASC, source_digest ASC`,
		factID,
	)
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list profile evidence: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var out []Evidence
	for rows.Next() {
		var evidence Evidence
		var provider, model, modelVersion, promptVersion, promptDigest sql.NullString
		var observed string
		if err := rows.Scan(
			&evidence.SourceEventID, &evidence.SourceDigest,
			&provider, &model, &modelVersion, &promptVersion, &promptDigest, &observed,
		); err != nil {
			return nil, sqlite.Classify(fmt.Errorf("scan profile evidence: %w", err))
		}
		evidence.Provider, evidence.Model = provider.String, model.String
		evidence.ModelVersion, evidence.PromptVersion = modelVersion.String, promptVersion.String
		evidence.PromptDigest = promptDigest.String
		observedAt, err := sqlite.ParseTime(observed)
		if err != nil {
			return nil, fmt.Errorf("parse profile evidence timestamp: %w", err)
		}
		evidence.ObservedAt = observedAt
		out = append(out, evidence)
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list profile evidence: %w", err))
	}
	return out, nil
}

func (s *SQLiteStore) ListActive(ctx context.Context, ownerID, category, key string) ([]Fact, error) {
	if s == nil || s.db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if strings.TrimSpace(ownerID) == "" || strings.TrimSpace(category) == "" || strings.TrimSpace(key) == "" {
		return nil, fault.New(ErrorCodeInvalidArgument, "profile lookup scope must not be empty")
	}
	conn := sqlite.Conn(ctx, s.db)
	rows, err := conn.QueryContext(ctx,
		`SELECT `+selectProfileFactColumns+`
		FROM profile_fact
		WHERE owner_id = ? AND category = ? AND fact_key = ? AND status = 'active'
		ORDER BY id ASC`,
		ownerID, category, key,
	)
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list active profile facts: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var out []Fact
	for rows.Next() {
		fact, err := scanProfileFact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, fact)
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list active profile facts: %w", err))
	}
	return out, nil
}

func (s *SQLiteStore) ListFacts(ctx context.Context, ownerID, status, category string, limit int) ([]Fact, error) {
	if s == nil || s.db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if strings.TrimSpace(ownerID) == "" {
		return nil, fault.New(ErrorCodeInvalidArgument, "owner must not be empty")
	}
	if limit <= 0 || limit > 10000 {
		return nil, fault.New(ErrorCodeInvalidArgument, "fact list limit is out of range")
	}
	sqlQuery := `SELECT ` + selectProfileFactColumns + ` FROM profile_fact WHERE owner_id = ?`
	args := []any{ownerID}
	if status != "" {
		if !profileStatuses[status] {
			return nil, fault.New(ErrorCodeProfileInvalid, "profile fact status is not valid")
		}
		sqlQuery += ` AND status = ?`
		args = append(args, status)
	}
	if category != "" {
		if !profileCategories[category] {
			return nil, fault.New(ErrorCodeProfileInvalid, "profile fact category is not valid")
		}
		sqlQuery += ` AND category = ?`
		args = append(args, category)
	}
	sqlQuery += ` ORDER BY category ASC, fact_key ASC, id ASC LIMIT ?`
	args = append(args, limit)
	conn := sqlite.Conn(ctx, s.db)
	rows, err := conn.QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list profile facts: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var out []Fact
	for rows.Next() {
		fact, err := scanProfileFact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, fact)
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list profile facts: %w", err))
	}
	return out, nil
}

type scoredProfileFact struct {
	fact  Fact
	rank  float64
	score float64
}

func scanScoredProfileFact(rows *sql.Rows) (scoredProfileFact, error) {
	var hit scoredProfileFact
	var verified int64
	var conflicts sql.NullString
	var expires sql.NullString
	var taintSource sql.NullString
	var created, updated string
	if err := rows.Scan(
		&hit.fact.ID, &hit.fact.OwnerID, &hit.fact.Category, &hit.fact.Key, &hit.fact.Value, &hit.fact.ValueDigest,
		&hit.fact.Status, &hit.fact.Origin, &verified, &hit.fact.Confidence, &hit.fact.ConfidencePolicy,
		&conflicts, &expires, &created, &updated, &taintSource, &hit.rank,
	); err != nil {
		return scoredProfileFact{}, sqlite.Classify(fmt.Errorf("scan profile fact: %w", err))
	}
	hit.fact.OwnerVerified = verified == 1
	hit.fact.ConflictsWithID = conflicts.String
	hit.fact.TaintSource = taintSource.String
	createdAt, err := sqlite.ParseTime(created)
	if err != nil {
		return scoredProfileFact{}, fmt.Errorf("parse profile fact timestamps: %w", err)
	}
	updatedAt, err := sqlite.ParseTime(updated)
	if err != nil {
		return scoredProfileFact{}, fmt.Errorf("parse profile fact timestamps: %w", err)
	}
	hit.fact.CreatedAt, hit.fact.UpdatedAt = createdAt, updatedAt
	if expires.Valid {
		expiresAt, err := sqlite.ParseTime(expires.String)
		if err != nil {
			return scoredProfileFact{}, fmt.Errorf("parse profile fact expiry: %w", err)
		}
		hit.fact.ExpiresAt = &expiresAt
	}
	return hit, nil
}

func rankProfileTier(hits []scoredProfileFact, now time.Time) {
	if len(hits) == 0 {
		return
	}
	maxMag := 0.0
	for i := range hits {
		if mag := textsearch.BM25Magnitude(hits[i].rank); mag > maxMag {
			maxMag = mag
		}
	}
	for i := range hits {
		bm25Norm := textsearch.NormalizeBM25(hits[i].rank, maxMag)
		recency := textsearch.RecencyScore(now, hits[i].fact.UpdatedAt, textsearch.DefaultHalfLifeDays)
		hits[i].score = textsearch.CombineScore(bm25Norm, recency)
	}
	slices.SortFunc(hits, func(a, b scoredProfileFact) int {
		if c := cmp.Compare(b.score, a.score); c != 0 {
			return c
		}
		return cmp.Compare(a.fact.ID, b.fact.ID)
	})
}

func (s *SQLiteStore) searchFactTier(ctx context.Context, ownerID, category, match string, verifiedOnly bool, want int, now time.Time) ([]scoredProfileFact, error) {
	sqlQuery := `SELECT f.id, f.owner_id, f.category, f.fact_key, f.fact_value, f.value_digest, f.status, f.origin,
		f.owner_verified, f.confidence, f.confidence_policy_version, f.conflicts_with_id,
		f.expires_at, f.created_at, f.updated_at, f.taint_source, bm25(profile_fact_fts) FROM profile_fact_fts
		JOIN profile_fact f ON f.rowid = profile_fact_fts.rowid
		WHERE profile_fact_fts MATCH ? AND f.owner_id = ? AND f.status = 'active'
		AND (f.expires_at IS NULL OR f.expires_at > ?)`
	nowUTC := now.UTC()
	args := []any{match, ownerID, sqlite.FormatTime(nowUTC.Add(-time.Second))}
	if verifiedOnly {
		sqlQuery += ` AND f.owner_verified = 1`
	}
	if category != "" {
		sqlQuery += ` AND f.category = ?`
		args = append(args, category)
	}
	sqlQuery += ` ORDER BY bm25(profile_fact_fts), f.confidence DESC, f.id ASC`
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("search profile facts: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var out []scoredProfileFact
	for rows.Next() {
		hit, err := scanScoredProfileFact(rows)
		if err != nil {
			return nil, err
		}
		if hit.fact.ExpiresAt != nil && !hit.fact.ExpiresAt.After(nowUTC) {
			continue
		}
		out = append(out, hit)
		if len(out) >= want {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("search profile facts: %w", err))
	}
	return out, nil
}

func (s *SQLiteStore) SearchFacts(ctx context.Context, ownerID, category, query string, verifiedOnly bool, limit int, now time.Time) ([]Fact, error) {
	if s == nil || s.db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if strings.TrimSpace(ownerID) == "" {
		return nil, fault.New(ErrorCodeInvalidArgument, "owner must not be empty")
	}
	if limit <= 0 || limit > 10000 {
		return nil, fault.New(ErrorCodeInvalidArgument, "fact search limit is out of range")
	}
	if now.IsZero() {
		return nil, fault.New(ErrorCodeInvalidArgument, "profile search timestamp must not be zero")
	}
	if category != "" && !profileCategories[category] {
		return nil, fault.New(ErrorCodeProfileInvalid, "profile fact category is not valid")
	}
	nowUTC := now.UTC()
	match := strings.TrimSpace(query)
	if match == "" {
		sqlQuery := `SELECT ` + selectProfileFactColumns + ` FROM profile_fact WHERE owner_id = ? AND status = 'active'
			AND (expires_at IS NULL OR expires_at > ?)`
		args := []any{ownerID, sqlite.FormatTime(nowUTC.Add(-time.Second))}
		if verifiedOnly {
			sqlQuery += ` AND owner_verified = 1`
		}
		if category != "" {
			sqlQuery += ` AND category = ?`
			args = append(args, category)
		}
		sqlQuery += ` ORDER BY confidence DESC, id ASC`
		rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx, sqlQuery, args...)
		if err != nil {
			return nil, sqlite.Classify(fmt.Errorf("search profile facts: %w", err))
		}
		defer func() { _ = rows.Close() }()
		var out []Fact
		for rows.Next() {
			fact, err := scanProfileFact(rows)
			if err != nil {
				return nil, err
			}
			if fact.ExpiresAt != nil && !fact.ExpiresAt.After(nowUTC) {
				continue
			}
			out = append(out, fact)
			if len(out) >= limit {
				break
			}
		}
		if err := rows.Err(); err != nil {
			return nil, sqlite.Classify(fmt.Errorf("search profile facts: %w", err))
		}
		return out, nil
	}
	rawTokens, err := tokenizeProfileQuery(match)
	if err != nil {
		return nil, err
	}
	meaningful := textsearch.FilterStopwords(rawTokens)
	candidateLimit := max(limit, min(max(limit*4, minProfileSearchCandidates), maxProfileSearchCandidates))
	stage1, err := s.searchFactTier(ctx, ownerID, category, textsearch.FTSAndQuery(meaningful), verifiedOnly, candidateLimit, nowUTC)
	if err != nil {
		return nil, err
	}
	rankProfileTier(stage1, nowUTC)
	combined := stage1
	needStage2 := len(meaningful) >= 2 && (len(stage1) < limit || (s.EmbeddingActive() && len(stage1) < candidateLimit))
	if needStage2 {
		stage2Raw, err := s.searchFactTier(ctx, ownerID, category, textsearch.FTSOrQuery(meaningful), verifiedOnly, candidateLimit, nowUTC)
		if err != nil {
			return nil, err
		}
		seen := make(map[string]struct{}, len(stage1))
		for i := range stage1 {
			seen[stage1[i].fact.ID] = struct{}{}
		}
		stage2 := make([]scoredProfileFact, 0, len(stage2Raw))
		for i := range stage2Raw {
			if _, exists := seen[stage2Raw[i].fact.ID]; exists {
				continue
			}
			stage2 = append(stage2, stage2Raw[i])
		}
		rankProfileTier(stage2, nowUTC)
		combined = append(combined, stage2...)
	}
	if semantic, ran, err := s.searchFactVectors(ctx, ownerID, category, match, candidateLimit, nowUTC); err != nil {
		s.logger.WarnContext(ctx, "profile vector search failed", "component", "profile_embedding", "error", err)
	} else if ran {
		combined = fuseProfileFacts(combined, semantic, candidateLimit)
	}
	if len(combined) > limit {
		combined = combined[:limit]
	}
	out := make([]Fact, 0, len(combined))
	for i := range combined {
		out = append(out, combined[i].fact)
	}
	return out, nil
}

func (s *SQLiteStore) InsertCandidate(ctx context.Context, fact *Fact, evidence *Evidence, minConfidence float64) (Fact, error) {
	if s == nil || s.db == nil {
		return Fact{}, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if fact == nil {
		return Fact{}, fault.New(ErrorCodeInvalidArgument, "profile fact must not be nil")
	}
	var stored Fact
	err := sqlite.RunInTx(ctx, s.db, func(ctx context.Context) error {
		conn := sqlite.Conn(ctx, s.db)
		stored = *fact
		if minConfidence > 0 {
			var conflicts sql.NullString
			if err := conn.QueryRowContext(ctx,
				`SELECT id FROM profile_fact
				 WHERE owner_id = ? AND category = ? AND fact_key = ? AND status = 'active' AND value_digest != ? AND id != ?
				 ORDER BY id ASC LIMIT 1`,
				fact.OwnerID, fact.Category, fact.Key, fact.ValueDigest, fact.ID,
			).Scan(&conflicts); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return sqlite.Classify(fmt.Errorf("read conflicting active fact: %w", err))
			} else if conflicts.Valid {
				stored.ConflictsWithID = conflicts.String
			}
		}
		if stored.ConflictsWithID == "" && stored.Confidence >= minConfidence && minConfidence > 0 {
			stored.Status = StatusActive
		}
		if err := s.InsertFact(ctx, &stored); err != nil {
			return err
		}
		if evidence != nil {
			if _, err := s.AddEvidence(ctx, stored.ID, evidence); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Fact{}, err
	}
	return stored, nil
}

func (s *SQLiteStore) Reinforce(ctx context.Context, factID string, evidence *Evidence, minConfidence float64) (Fact, error) {
	if s == nil || s.db == nil {
		return Fact{}, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if evidence == nil {
		return Fact{}, fault.New(ErrorCodeInvalidArgument, "profile evidence must not be nil")
	}
	var updated Fact
	err := sqlite.RunInTx(ctx, s.db, func(ctx context.Context) error {
		conn := sqlite.Conn(ctx, s.db)
		fact, found, err := s.GetFact(ctx, factID)
		if err != nil {
			return err
		}
		if !found {
			return fault.New(ErrorCodeProfileNotFound, "fact does not exist")
		}
		if fact.Status != StatusCandidate && fact.Status != StatusActive {
			return fault.New(ErrorCodeProfileConflict, "fact is not reinforceable")
		}
		if _, err := s.AddEvidence(ctx, fact.ID, evidence); err != nil {
			return err
		}
		var count int
		if err := conn.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM profile_evidence WHERE fact_id = ?`, factID,
		).Scan(&count); err != nil {
			return sqlite.Classify(fmt.Errorf("count profile evidence: %w", err))
		}
		fact.Confidence = ConfidenceV1(count)
		if !fact.OwnerVerified && fact.TaintSource == "" && evidence.TaintSource != "" {
			fact.TaintSource = evidence.TaintSource
		}
		if fact.Status == StatusCandidate && fact.Confidence >= minConfidence {
			var conflicts sql.NullString
			if err := conn.QueryRowContext(ctx,
				`SELECT id FROM profile_fact
				 WHERE owner_id = ? AND category = ? AND fact_key = ? AND status = 'active' AND value_digest != ? AND id != ?
				 ORDER BY id ASC LIMIT 1`,
				fact.OwnerID, fact.Category, fact.Key, fact.ValueDigest, fact.ID,
			).Scan(&conflicts); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return sqlite.Classify(fmt.Errorf("read conflicting active fact: %w", err))
			} else if conflicts.Valid {
				fact.ConflictsWithID = conflicts.String
			} else {
				fact.Status = StatusActive
			}
		}
		if err := s.UpdateFact(ctx, &fact); err != nil {
			return err
		}
		updated = fact
		return nil
	})
	if err != nil {
		return Fact{}, err
	}
	return updated, nil
}

func (s *SQLiteStore) SetState(ctx context.Context, factID, status, conflictsWith string, confidence float64, verified bool, at time.Time) error {
	if s == nil || s.db == nil {
		return fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	return sqlite.RunInTx(ctx, s.db, func(ctx context.Context) error {
		fact, found, err := s.GetFact(ctx, factID)
		if err != nil {
			return err
		}
		if !found {
			return fault.New(ErrorCodeProfileNotFound, "fact does not exist")
		}
		fact.Status = status
		fact.ConflictsWithID = conflictsWith
		fact.Confidence = confidence
		fact.OwnerVerified = verified
		if status == StatusActive && verified {
			fact.TaintSource = ""
		}
		fact.UpdatedAt = at
		return s.UpdateFact(ctx, &fact)
	})
}

func (s *SQLiteStore) Promote(ctx context.Context, factID string, expiresAt *time.Time, at time.Time) error {
	if s == nil || s.db == nil {
		return fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	return sqlite.RunInTx(ctx, s.db, func(ctx context.Context) error {
		fact, found, err := s.GetFact(ctx, factID)
		if err != nil {
			return err
		}
		if !found {
			return fault.New(ErrorCodeProfileNotFound, "fact does not exist")
		}
		fact.Status = StatusActive
		fact.ConflictsWithID = ""
		fact.Confidence = 1.0
		fact.OwnerVerified = true
		fact.TaintSource = ""
		fact.Origin = OriginOwner
		fact.ExpiresAt = expiresAt
		fact.UpdatedAt = at
		return s.UpdateFact(ctx, &fact)
	})
}

func (s *SQLiteStore) ReviveCandidate(ctx context.Context, factID string, at time.Time) error {
	if s == nil || s.db == nil {
		return fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	return sqlite.RunInTx(ctx, s.db, func(ctx context.Context) error {
		fact, found, err := s.GetFact(ctx, factID)
		if err != nil {
			return err
		}
		if !found {
			return fault.New(ErrorCodeProfileNotFound, "fact does not exist")
		}
		fact.Status = StatusCandidate
		fact.UpdatedAt = at
		return s.UpdateFact(ctx, &fact)
	})
}

func (s *SQLiteStore) Get(ctx context.Context, id string) (Fact, bool, error) {
	return s.GetFact(ctx, id)
}

func (s *SQLiteStore) Active(ctx context.Context, ownerID, category, key string) ([]Fact, error) {
	return s.ListActive(ctx, ownerID, category, key)
}

func (s *SQLiteStore) Evidence(ctx context.Context, factID string) ([]Evidence, error) {
	return s.ListEvidence(ctx, factID)
}

func (s *SQLiteStore) Expire(ctx context.Context, now time.Time) ([]Fact, error) {
	if s == nil || s.db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	var expired []Fact
	err := sqlite.RunInTx(ctx, s.db, func(ctx context.Context) error {
		conn := sqlite.Conn(ctx, s.db)
		ids, err := s.expireCandidateIDs(ctx, now)
		if err != nil {
			return err
		}
		expired = make([]Fact, 0, len(ids))
		for _, id := range ids {
			stored, ok, err := s.GetFact(ctx, id)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			expired = append(expired, stored)
		}
		formatted := sqlite.FormatTime(now.UTC())
		if _, err := conn.ExecContext(ctx,
			`UPDATE profile_fact SET status = 'expired', updated_at = ?
			 WHERE status IN ('candidate','active') AND expires_at IS NOT NULL AND expires_at <= ?`,
			formatted, formatted,
		); err != nil {
			return sqlite.Classify(fmt.Errorf("expire profile facts: %w", err))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for i := range expired {
		expired[i].Status = StatusExpired
		expired[i].UpdatedAt = now
	}
	return expired, nil
}

func (s *SQLiteStore) expireCandidateIDs(ctx context.Context, now time.Time) ([]string, error) {
	conn := sqlite.Conn(ctx, s.db)
	rows, err := conn.QueryContext(ctx,
		`SELECT id FROM profile_fact
		 WHERE status IN ('candidate','active') AND expires_at IS NOT NULL AND expires_at <= ?
		 ORDER BY id`, sqlite.FormatTime(now.UTC()),
	)
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("expire profile facts: %w", err))
	}
	defer func() { _ = rows.Close() }()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, sqlite.Classify(fmt.Errorf("expire profile facts: %w", err))
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("expire profile facts: %w", err))
	}
	return ids, nil
}

func (s *SQLiteStore) Search(ctx context.Context, ownerID, category, query string, verifiedOnly bool, limit int, now time.Time) ([]SearchHit, error) {
	rows, err := s.SearchFacts(ctx, ownerID, category, query, verifiedOnly, limit, now)
	if err != nil {
		return nil, err
	}
	out := make([]SearchHit, 0, len(rows))
	for i := range rows {
		out = append(out, SearchHit{Fact: rows[i]})
	}
	return out, nil
}

func (s *SQLiteStore) List(ctx context.Context, ownerID, status, category string, limit int) ([]Fact, error) {
	return s.ListFacts(ctx, ownerID, status, category, limit)
}

func (s *SQLiteStore) FactCounts(ctx context.Context) ([]FactCount, error) {
	if s == nil || s.db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	conn := sqlite.Conn(ctx, s.db)
	rows, err := conn.QueryContext(ctx, `SELECT status, category, COUNT(*) FROM profile_fact GROUP BY status, category`)
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("count profile facts: %w", err))
	}
	defer func() { _ = rows.Close() }()
	counts := []FactCount{}
	for rows.Next() {
		var count FactCount
		if err := rows.Scan(&count.Status, &count.Category, &count.Count); err != nil {
			return nil, sqlite.Classify(fmt.Errorf("scan profile fact count: %w", err))
		}
		counts = append(counts, count)
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("count profile facts: %w", err))
	}
	return counts, nil
}

func (s *SQLiteStore) RebuildFTS(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	conn := sqlite.Conn(ctx, s.db)
	if _, err := conn.ExecContext(ctx, `INSERT INTO profile_fact_fts(profile_fact_fts) VALUES ('rebuild')`); err != nil {
		return sqlite.Classify(fmt.Errorf("rebuild profile index: %w", err))
	}
	return nil
}
