package profile

import (
	"cmp"
	stdcontext "context"
	"database/sql"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/embedding"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/secret"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/textsearch"
)

const upsertProfileEmbeddingSQL = `INSERT INTO profile_embedding (fact_id, fingerprint, content_sha256, vector, created_at)
	SELECT f.id, ?, ?, ?, ?
	FROM profile_fact f
	WHERE f.id = ? AND f.status = 'active' AND f.owner_verified = 1
	ON CONFLICT(fact_id) DO UPDATE SET
		fingerprint = excluded.fingerprint,
		content_sha256 = excluded.content_sha256,
		vector = excluded.vector,
		created_at = excluded.created_at`

type pendingProfileFact struct {
	ID        string
	Category  string
	Key       string
	Value     string
	UpdatedAt string
}

type profileEmbeddingWrite struct {
	FactID        string
	ContentSHA256 string
	Vector        []float32
	CreatedAt     time.Time
}

func NewSQLiteStoreWithEmbedding(db *sql.DB, opts *EmbeddingOptions) (*SQLiteStore, error) {
	store, err := NewSQLiteStore(db)
	if err != nil {
		return nil, err
	}
	if opts != nil {
		copied := *opts
		if copied.MaxVectorScanRows <= 0 {
			copied.MaxVectorScanRows = maxVectorScanRows
		}
		store.embedding = &copied
	}
	return store, nil
}

func (s *SQLiteStore) EmbeddingActive() bool {
	return s != nil && s.embedding != nil && s.embedding.Enabled && s.embedding.Embedder != nil && s.embedding.Settings.valid()
}

func (s *SQLiteStore) embedScreenSnapshot(ctx stdcontext.Context) (secret.Screen, error) {
	if s.embedding != nil && s.embedding.SecretSource != nil {
		return s.embedding.SecretSource.Snapshot(ctx)
	}
	if s.embedding != nil && s.embedding.Scanner != nil {
		return s.embedding.Scanner, nil
	}
	return rejectAllScanner{}, nil
}

func (s *SQLiteStore) textBlockedForEmbed(text string, screen secret.Screen) bool {
	if _, matched := secret.MatchSensitive(text, screen); matched {
		return true
	}
	if s.embedding != nil && s.embedding.Scanner != nil && s.embedding.Scanner.Contains(text) {
		return true
	}
	return false
}

func (s *SQLiteStore) factBlockedForEmbed(category, key, value string, screen secret.Screen) bool {
	text := factEmbedText(key, value)
	if s.textBlockedForEmbed(text, screen) {
		return true
	}
	if SensitiveReason(category, key, value, screen) != "" {
		return true
	}
	return false
}

func (s *SQLiteStore) upsertFactEmbeddings(ctx stdcontext.Context, fingerprint string, writes []profileEmbeddingWrite) error {
	if s == nil || s.db == nil {
		return fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if fingerprint == "" || len(writes) == 0 {
		return fault.New(ErrorCodeInvalidArgument, "embedding fingerprint and writes are required")
	}
	for i := range writes {
		if writes[i].FactID == "" || len(writes[i].ContentSHA256) != 64 {
			return fault.New(ErrorCodeInvalidArgument, "profile embedding requires a fact id and sha256 digest")
		}
		if err := embedding.ValidateUnit(writes[i].Vector); err != nil {
			return fault.Wrap(ErrorCodeInvalidArgument, "profile embedding vector is not a valid unit vector", err)
		}
	}
	return sqlite.RunInTx(ctx, s.db, func(txCtx stdcontext.Context) error {
		conn := sqlite.Conn(txCtx, s.db)
		for i := range writes {
			createdAt := writes[i].CreatedAt.UTC()
			if createdAt.IsZero() {
				createdAt = time.Now().UTC()
			}
			if _, err := conn.ExecContext(
				txCtx,
				upsertProfileEmbeddingSQL,
				fingerprint,
				writes[i].ContentSHA256,
				embedding.Encode(writes[i].Vector),
				sqlite.FormatTime(createdAt),
				writes[i].FactID,
			); err != nil {
				return sqlite.Classify(fmt.Errorf("upsert profile embedding: %w", err))
			}
		}
		return nil
	})
}

func (s *SQLiteStore) EmbedFactBestEffort(ctx stdcontext.Context, fact *Fact) {
	if !s.EmbeddingActive() || fact == nil || fact.Status != StatusActive || !fact.OwnerVerified {
		return
	}
	screen, err := s.embedScreenSnapshot(ctx)
	if err != nil || s.factBlockedForEmbed(fact.Category, fact.Key, fact.Value, screen) {
		return
	}
	text := factEmbedText(fact.Key, fact.Value)
	embedCtx, cancel := stdcontext.WithTimeout(ctx, postWriteEmbedTimeout)
	defer cancel()
	vectors, err := s.embedding.Embedder.Embed(embedCtx, embedding.RoleDocument, []string{text})
	if err != nil || len(vectors) != 1 {
		return
	}
	unit, ok := embedding.Normalize(vectors[0], s.embedding.Settings.Dimensions)
	if !ok {
		return
	}
	_ = s.upsertFactEmbeddings(ctx, s.embedding.Settings.Fingerprint, []profileEmbeddingWrite{{
		FactID:        fact.ID,
		ContentSHA256: factContentSHA256(text),
		Vector:        unit,
		CreatedAt:     time.Now().UTC(),
	}})
}

func (s *SQLiteStore) pendingFactsForEmbedding(ctx stdcontext.Context, fingerprint, afterUpdatedAt, afterID string, now time.Time, limit int) ([]pendingProfileFact, error) {
	sqlQuery := `SELECT f.id, f.category, f.fact_key, f.fact_value, f.updated_at
		FROM profile_fact f
		LEFT JOIN profile_embedding e ON e.fact_id = f.id AND e.fingerprint = ?
		WHERE f.status = 'active' AND f.owner_verified = 1
		AND (f.expires_at IS NULL OR f.expires_at > ?)
		AND e.fact_id IS NULL`
	args := []any{fingerprint, sqlite.FormatTime(now.UTC().Add(-time.Second))}
	if afterUpdatedAt != "" {
		sqlQuery += ` AND (f.updated_at < ? OR (f.updated_at = ? AND f.id > ?))`
		args = append(args, afterUpdatedAt, afterUpdatedAt, afterID)
	}
	sqlQuery += ` ORDER BY f.updated_at DESC, f.id ASC LIMIT ?`
	args = append(args, limit)
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list pending profile embeddings: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var out []pendingProfileFact
	for rows.Next() {
		var row pendingProfileFact
		if err := rows.Scan(&row.ID, &row.Category, &row.Key, &row.Value, &row.UpdatedAt); err != nil {
			return nil, sqlite.Classify(fmt.Errorf("scan pending profile embedding: %w", err))
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list pending profile embeddings: %w", err))
	}
	return out, nil
}

func (s *SQLiteStore) BackfillEmbeddingsOnce(ctx stdcontext.Context, maxRows int) (int, error) {
	if !s.EmbeddingActive() {
		return 0, nil
	}
	if maxRows <= 0 {
		maxRows = maxBackfillRowsPerTick
	}
	batchSize := min(s.embedding.Settings.batchSize(), maxRows)
	var cursorUpdatedAt, cursorID string
	embedded := 0
	scanned := 0
	now := time.Now().UTC()
	for scanned < maxRows {
		if err := ctx.Err(); err != nil {
			return embedded, err
		}
		remaining := min(batchSize, maxRows-scanned)
		pending, err := s.pendingFactsForEmbedding(ctx, s.embedding.Settings.Fingerprint, cursorUpdatedAt, cursorID, now, remaining)
		if err != nil {
			return embedded, err
		}
		if len(pending) == 0 {
			return embedded, nil
		}
		last := pending[len(pending)-1]
		cursorUpdatedAt, cursorID = last.UpdatedAt, last.ID
		scanned += len(pending)

		screen, err := s.embedScreenSnapshot(ctx)
		if err != nil {
			return embedded, err
		}
		eligible := make([]pendingProfileFact, 0, len(pending))
		texts := make([]string, 0, len(pending))
		for i := range pending {
			row := &pending[i]
			if s.factBlockedForEmbed(row.Category, row.Key, row.Value, screen) {
				continue
			}
			eligible = append(eligible, *row)
			texts = append(texts, factEmbedText(row.Key, row.Value))
		}
		if len(eligible) == 0 {
			continue
		}
		vectors, err := s.embedding.Embedder.Embed(ctx, embedding.RoleDocument, texts)
		if err != nil {
			return embedded, err
		}
		if len(vectors) != len(eligible) {
			return embedded, nil
		}
		writes := make([]profileEmbeddingWrite, 0, len(eligible))
		writeAt := time.Now().UTC()
		for i := range eligible {
			unit, ok := embedding.Normalize(vectors[i], s.embedding.Settings.Dimensions)
			if !ok {
				continue
			}
			writes = append(writes, profileEmbeddingWrite{
				FactID:        eligible[i].ID,
				ContentSHA256: factContentSHA256(texts[i]),
				Vector:        unit,
				CreatedAt:     writeAt,
			})
		}
		if len(writes) == 0 {
			continue
		}
		if err := s.upsertFactEmbeddings(ctx, s.embedding.Settings.Fingerprint, writes); err != nil {
			return embedded, err
		}
		embedded += len(writes)
	}
	return embedded, nil
}

func (s *SQLiteStore) RunEmbeddingBackfill(ctx stdcontext.Context, interval time.Duration) {
	if !s.EmbeddingActive() {
		return
	}
	if interval <= 0 {
		interval = defaultBackfillInterval
	}
	_, _ = s.BackfillEmbeddingsOnce(ctx, maxBackfillRowsPerTick)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = s.BackfillEmbeddingsOnce(ctx, maxBackfillRowsPerTick)
		}
	}
}

func (s *SQLiteStore) hasFactEmbeddings(ctx stdcontext.Context, ownerID, category, fingerprint string, nowUTC time.Time) (bool, error) {
	sqlQuery := `SELECT EXISTS (
		SELECT 1 FROM profile_embedding e
		JOIN profile_fact f ON f.id = e.fact_id
		WHERE e.fingerprint = ? AND f.owner_id = ? AND f.status = 'active' AND f.owner_verified = 1
		AND (f.expires_at IS NULL OR f.expires_at > ?)`
	args := []any{fingerprint, ownerID, sqlite.FormatTime(nowUTC.Add(-time.Second))}
	if category != "" {
		sqlQuery += ` AND f.category = ?`
		args = append(args, category)
	}
	sqlQuery += `)`
	var exists bool
	if err := sqlite.Conn(ctx, s.db).QueryRowContext(ctx, sqlQuery, args...).Scan(&exists); err != nil {
		return false, sqlite.Classify(fmt.Errorf("check profile embeddings: %w", err))
	}
	return exists, nil
}

func betterProfileHit(a, b *scoredProfileFact) bool {
	return a.score > b.score || (a.score == b.score && a.fact.ID < b.fact.ID)
}

func offerProfileHit(top *[]scoredProfileFact, limit int, candidate *scoredProfileFact) {
	items := *top
	if len(items) == limit && !betterProfileHit(candidate, &items[len(items)-1]) {
		return
	}
	at, _ := slices.BinarySearchFunc(items, *candidate, func(existing, target scoredProfileFact) int {
		switch {
		case betterProfileHit(&existing, &target):
			return -1
		case betterProfileHit(&target, &existing):
			return 1
		default:
			return 0
		}
	})
	if len(items) < limit {
		items = append(items, scoredProfileFact{})
	}
	copy(items[at+1:], items[at:len(items)-1])
	items[at] = *candidate
	*top = items
}

func (s *SQLiteStore) searchFactVectors(ctx stdcontext.Context, ownerID, category, query string, candidateLimit int, nowUTC time.Time) ([]scoredProfileFact, bool) {
	if !s.EmbeddingActive() || len(query) > MaxEmbedQueryBytes || strings.TrimSpace(query) == "" {
		return nil, false
	}
	has, err := s.hasFactEmbeddings(ctx, ownerID, category, s.embedding.Settings.Fingerprint, nowUTC)
	if err != nil || !has {
		return nil, false
	}
	screen, err := s.embedScreenSnapshot(ctx)
	if err != nil || s.textBlockedForEmbed(query, screen) {
		return nil, false
	}
	embedCtx, cancel := stdcontext.WithTimeout(ctx, s.embedding.Settings.queryTimeout())
	defer cancel()
	vectors, err := s.embedding.Embedder.Embed(embedCtx, embedding.RoleQuery, []string{query})
	if err != nil || len(vectors) != 1 {
		return nil, false
	}
	queryVec, ok := embedding.Normalize(vectors[0], s.embedding.Settings.Dimensions)
	if !ok {
		return nil, false
	}
	sqlQuery := `SELECT f.id, f.owner_id, f.category, f.fact_key, f.fact_value, f.value_digest, f.status, f.origin,
		f.owner_verified, f.confidence, f.confidence_policy_version, f.conflicts_with_id,
		f.expires_at, f.created_at, f.updated_at, f.taint_source, e.content_sha256, e.vector
		FROM profile_embedding e
		JOIN profile_fact f ON f.id = e.fact_id
		WHERE e.fingerprint = ? AND f.owner_id = ? AND f.status = 'active' AND f.owner_verified = 1
		AND (f.expires_at IS NULL OR f.expires_at > ?)`
	args := []any{s.embedding.Settings.Fingerprint, ownerID, sqlite.FormatTime(nowUTC.Add(-time.Second))}
	if category != "" {
		sqlQuery += ` AND f.category = ?`
		args = append(args, category)
	}
	sqlQuery += ` ORDER BY f.updated_at DESC, f.id ASC`
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		return nil, false
	}
	defer func() { _ = rows.Close() }()

	maxScan := s.embedding.MaxVectorScanRows
	if maxScan <= 0 {
		maxScan = maxVectorScanRows
	}
	top := make([]scoredProfileFact, 0, min(candidateLimit, maxProfileSearchCandidates))
	scanned := 0
	for rows.Next() {
		if scanned >= maxScan {
			break
		}
		if scanned%vectorScanCancelInterval == 0 {
			if err := ctx.Err(); err != nil {
				return nil, false
			}
		}
		scanned++
		fact, digest, blob, err := scanProfileVectorRow(rows)
		if err != nil {
			return nil, false
		}
		if fact.ExpiresAt != nil && !fact.ExpiresAt.After(nowUTC) {
			continue
		}
		if factContentSHA256(factEmbedText(fact.Key, fact.Value)) != digest {
			continue
		}
		vec, err := embedding.Decode(blob, s.embedding.Settings.Dimensions)
		if err != nil {
			continue
		}
		dot := float64(embedding.Dot(queryVec, vec))
		if dot <= 0 || math.IsNaN(dot) || math.IsInf(dot, 0) {
			continue
		}
		cosine := min(dot, 1.0)
		recency := textsearch.RecencyScore(nowUTC, fact.UpdatedAt, textsearch.DefaultHalfLifeDays)
		candidate := scoredProfileFact{
			fact:  fact,
			score: semanticCosineWeight*cosine + semanticRecencyWeight*recency,
		}
		offerProfileHit(&top, candidateLimit, &candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, false
	}
	return top, true
}

func scanProfileVectorRow(rows *sql.Rows) (fact Fact, digest string, blob []byte, err error) {
	var verified int64
	var conflicts sql.NullString
	var expires sql.NullString
	var taintSource sql.NullString
	var created, updated string
	if err := rows.Scan(
		&fact.ID, &fact.OwnerID, &fact.Category, &fact.Key, &fact.Value, &fact.ValueDigest,
		&fact.Status, &fact.Origin, &verified, &fact.Confidence, &fact.ConfidencePolicy,
		&conflicts, &expires, &created, &updated, &taintSource, &digest, &blob,
	); err != nil {
		return Fact{}, "", nil, sqlite.Classify(fmt.Errorf("scan profile vector row: %w", err))
	}
	fact.OwnerVerified = verified == 1
	fact.ConflictsWithID = conflicts.String
	fact.TaintSource = taintSource.String
	createdAt, err := sqlite.ParseTime(created)
	if err != nil {
		return Fact{}, "", nil, fmt.Errorf("parse profile fact timestamps: %w", err)
	}
	updatedAt, err := sqlite.ParseTime(updated)
	if err != nil {
		return Fact{}, "", nil, fmt.Errorf("parse profile fact timestamps: %w", err)
	}
	fact.CreatedAt, fact.UpdatedAt = createdAt, updatedAt
	if expires.Valid {
		expiresAt, err := sqlite.ParseTime(expires.String)
		if err != nil {
			return Fact{}, "", nil, fmt.Errorf("parse profile fact expiry: %w", err)
		}
		fact.ExpiresAt = &expiresAt
	}
	return fact, digest, blob, nil
}

func fuseProfileFacts(lexical, semantic []scoredProfileFact, depth int) []scoredProfileFact {
	byID := make(map[string]scoredProfileFact, len(lexical)+len(semantic))
	lexIDs := make([]string, len(lexical))
	for i := range lexical {
		lexIDs[i] = lexical[i].fact.ID
		if _, exists := byID[lexical[i].fact.ID]; !exists {
			byID[lexical[i].fact.ID] = lexical[i]
		}
	}
	semIDs := make([]string, len(semantic))
	for i := range semantic {
		semIDs[i] = semantic[i].fact.ID
		if _, exists := byID[semantic[i].fact.ID]; !exists {
			byID[semantic[i].fact.ID] = semantic[i]
		}
	}
	items := embedding.FuseRRF(lexIDs, semIDs, depth, cmp.Compare[string])
	out := make([]scoredProfileFact, len(items))
	for i, item := range items {
		hit := byID[item.Key]
		hit.score = item.Score
		out[i] = hit
	}
	return out
}

func (s *SQLiteStore) EmbeddingCoverage(ctx stdcontext.Context, fingerprint string, now time.Time) (EmbeddingCoverage, error) {
	if s == nil || s.db == nil {
		return EmbeddingCoverage{}, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var cov EmbeddingCoverage
	err := sqlite.Conn(ctx, s.db).QueryRowContext(ctx,
		`SELECT count(f.id),
		 coalesce(sum(CASE WHEN e.fingerprint = ? THEN 1 ELSE 0 END), 0),
		 coalesce(sum(CASE WHEN e.fact_id IS NOT NULL AND e.fingerprint != ? THEN 1 ELSE 0 END), 0)
		 FROM profile_fact f
		 LEFT JOIN profile_embedding e ON e.fact_id = f.id
		 WHERE f.status = 'active' AND f.owner_verified = 1
		 AND (f.expires_at IS NULL OR f.expires_at > ?)`,
		fingerprint, fingerprint, sqlite.FormatTime(now.UTC().Add(-time.Second)),
	).Scan(&cov.Total, &cov.Embedded, &cov.Stale)
	if err != nil {
		return EmbeddingCoverage{}, sqlite.Classify(fmt.Errorf("count profile embedding coverage: %w", err))
	}
	return cov, nil
}
