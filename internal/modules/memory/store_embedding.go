package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/anggasct/aura/internal/platform/embedding"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/textsearch"
)

const upsertMemoryEmbeddingSQL = `INSERT INTO memory_embedding (document_id, fingerprint, content_sha256, vector, created_at)
	SELECT d.id, ?, ?, ?, ?
	FROM memory_document d
	WHERE d.id = ?
	ON CONFLICT(document_id) DO UPDATE SET
		fingerprint = excluded.fingerprint,
		content_sha256 = excluded.content_sha256,
		vector = excluded.vector,
		created_at = excluded.created_at`

func (s *SQLiteStore) UpsertEmbeddings(ctx context.Context, fingerprint string, writes []EmbeddingWrite) error {
	if s == nil || s.db == nil {
		return fault.New(ErrorCodeInvalidArgument, "db must not be nil")
	}
	if fingerprint == "" || len(writes) == 0 {
		return fault.New(ErrorCodeInvalidArgument, "embedding fingerprint and writes are required")
	}
	for i := range writes {
		if writes[i].DocumentID == "" || len(writes[i].ContentSHA256) != 64 {
			return fault.New(ErrorCodeInvalidArgument, "memory embedding requires a document id and sha256 digest")
		}
		if err := embedding.ValidateUnit(writes[i].Vector); err != nil {
			return fault.Wrap(ErrorCodeInvalidArgument, "memory embedding vector is not a valid unit vector", err)
		}
	}
	return sqlite.RunInTx(ctx, s.db, func(txCtx context.Context) error {
		conn := sqlite.Conn(txCtx, s.db)
		for i := range writes {
			createdAt := writes[i].CreatedAt.UTC()
			if createdAt.IsZero() {
				createdAt = time.Now().UTC()
			}
			if _, err := conn.ExecContext(
				txCtx,
				upsertMemoryEmbeddingSQL,
				fingerprint,
				writes[i].ContentSHA256,
				embedding.Encode(writes[i].Vector),
				sqlite.FormatTime(createdAt),
				writes[i].DocumentID,
			); err != nil {
				return sqlite.Classify(fmt.Errorf("upsert memory embedding: %w", err))
			}
		}
		return nil
	})
}

func (s *SQLiteStore) PendingEmbeddings(
	ctx context.Context,
	fingerprint string,
	includeNonOwner bool,
	afterCreatedAt, afterID string,
	now time.Time,
	limit int,
) ([]PendingEmbedding, error) {
	if s == nil || s.db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "db must not be nil")
	}
	if fingerprint == "" || limit <= 0 {
		return nil, fault.New(ErrorCodeInvalidArgument, "pending memory embeddings need a fingerprint and positive limit")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	sqlQuery := `SELECT d.id, d.owner_id, d.content, d.created_at
		FROM memory_document d
		LEFT JOIN memory_embedding e ON e.document_id = d.id AND e.fingerprint = ?
		WHERE e.document_id IS NULL AND (d.expires_at IS NULL OR d.expires_at > ?)`
	args := []any{fingerprint, sqlite.FormatTime(now.UTC().Add(-time.Second))}
	if !includeNonOwner {
		sqlQuery += ` AND d.owner_id = ?`
		args = append(args, principal.OwnerID)
	}
	if afterCreatedAt != "" {
		sqlQuery += ` AND (d.created_at < ? OR (d.created_at = ? AND d.id > ?))`
		args = append(args, afterCreatedAt, afterCreatedAt, afterID)
	}
	sqlQuery += ` ORDER BY d.created_at DESC, d.id ASC LIMIT ?`
	args = append(args, limit)
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list pending memory embeddings: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var out []PendingEmbedding
	for rows.Next() {
		var row PendingEmbedding
		if err := rows.Scan(&row.DocumentID, &row.OwnerID, &row.Content, &row.CreatedAt); err != nil {
			return nil, sqlite.Classify(fmt.Errorf("scan pending memory embedding: %w", err))
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list pending memory embeddings: %w", err))
	}
	return out, nil
}

func appendVectorScopeSQL(sqlQuery string, args []any, query *VectorQuery, now time.Time) (outSQL string, outArgs []any, err error) {
	if len(query.AgentIDs) > 0 {
		readable, err := json.Marshal(query.AgentIDs)
		if err != nil {
			return "", nil, fault.New(ErrorCodeInvalidArgument, "search personas are not encodable")
		}
		sqlQuery += ` AND d.agent_id IN (SELECT value FROM json_each(?))`
		args = append(args, string(readable))
	}
	if query.SessionID != "" {
		sqlQuery += ` AND d.session_id = ?`
		args = append(args, query.SessionID)
	}
	if !query.After.IsZero() {
		sqlQuery += ` AND d.created_at >= ?`
		args = append(args, sqlite.FormatTime(query.After.UTC().Add(-time.Second)))
	}
	if !query.Before.IsZero() {
		sqlQuery += ` AND d.created_at <= ?`
		args = append(args, sqlite.FormatTime(query.Before.UTC().Add(time.Second)))
	}
	sqlQuery += ` AND (d.expires_at IS NULL OR d.expires_at > ?)`
	args = append(args, sqlite.FormatTime(now.Add(-time.Second)))
	return sqlQuery, args, nil
}

func (s *SQLiteStore) HasEmbeddings(ctx context.Context, query *VectorQuery) (bool, error) {
	if s == nil || s.db == nil {
		return false, fault.New(ErrorCodeInvalidArgument, "db must not be nil")
	}
	if query == nil || query.OwnerID == "" || query.Fingerprint == "" {
		return false, fault.New(ErrorCodeInvalidArgument, "vector scope needs an owner and fingerprint")
	}
	now := query.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	sqlQuery := `SELECT EXISTS (SELECT 1 FROM memory_embedding e JOIN memory_document d ON d.id = e.document_id WHERE e.fingerprint = ? AND d.owner_id = ?`
	args := []any{query.Fingerprint, query.OwnerID}
	var err error
	sqlQuery, args, err = appendVectorScopeSQL(sqlQuery, args, query, now)
	if err != nil {
		return false, err
	}
	sqlQuery += `)`
	var exists bool
	if err := sqlite.Conn(ctx, s.db).QueryRowContext(ctx, sqlQuery, args...).Scan(&exists); err != nil {
		return false, sqlite.Classify(fmt.Errorf("check memory embeddings: %w", err))
	}
	return exists, nil
}

func betterMemoryHit(a, b *StoredHit) bool {
	return a.Score > b.Score || (a.Score == b.Score && a.ID < b.ID)
}

func offerMemoryHit(top *[]StoredHit, limit int, candidate *StoredHit) {
	items := *top
	if len(items) == limit && !betterMemoryHit(candidate, &items[len(items)-1]) {
		return
	}
	at, _ := slices.BinarySearchFunc(items, *candidate, func(existing, target StoredHit) int {
		switch {
		case betterMemoryHit(&existing, &target):
			return -1
		case betterMemoryHit(&target, &existing):
			return 1
		default:
			return 0
		}
	})
	if len(items) < limit {
		items = append(items, StoredHit{})
	}
	copy(items[at+1:], items[at:len(items)-1])
	items[at] = *candidate
	*top = items
}

func (s *SQLiteStore) SearchVectors(ctx context.Context, query *VectorQuery) (VectorSearchResult, error) {
	if s == nil || s.db == nil {
		return VectorSearchResult{}, fault.New(ErrorCodeInvalidArgument, "db must not be nil")
	}
	if query == nil || query.OwnerID == "" || query.Fingerprint == "" || query.Dimensions <= 0 || query.Limit <= 0 {
		return VectorSearchResult{}, fault.New(ErrorCodeInvalidArgument, "vector search query is not valid")
	}
	if len(query.QueryVector) != query.Dimensions {
		return VectorSearchResult{}, fault.New(ErrorCodeInvalidArgument, "query vector dimensions do not match")
	}
	if !query.After.IsZero() && !query.Before.IsZero() && query.After.After(query.Before) {
		return VectorSearchResult{}, fault.New(ErrorCodeInvalidArgument, "search after timestamp must not be after before timestamp")
	}
	now := query.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	maxScan := query.MaxScanRows
	if maxScan <= 0 {
		maxScan = maxVectorScanRows
	}
	sqlQuery := `SELECT ` + selectMemoryDocumentColumns + `, e.content_sha256, e.vector
		FROM memory_embedding e
		JOIN memory_document d ON d.id = e.document_id
		WHERE e.fingerprint = ? AND d.owner_id = ?`
	args := []any{query.Fingerprint, query.OwnerID}
	var err error
	sqlQuery, args, err = appendVectorScopeSQL(sqlQuery, args, query, now)
	if err != nil {
		return VectorSearchResult{}, err
	}
	sqlQuery += ` ORDER BY d.created_at DESC, d.id ASC`
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		return VectorSearchResult{}, sqlite.Classify(fmt.Errorf("search memory vectors: %w", err))
	}
	defer func() { _ = rows.Close() }()

	top := make([]StoredHit, 0, min(query.Limit, maxSearchCandidateRows))
	scanned := 0
	for rows.Next() {
		if scanned >= maxScan {
			break
		}
		if scanned%vectorScanCancelInterval == 0 {
			if err := ctx.Err(); err != nil {
				return VectorSearchResult{}, err
			}
		}
		scanned++
		doc, digest, blob, err := scanMemoryVectorRow(rows)
		if err != nil {
			return VectorSearchResult{}, err
		}
		if doc.ExpiresAt != nil && !doc.ExpiresAt.After(now) {
			continue
		}
		if !query.After.IsZero() && doc.CreatedAt.Before(query.After) {
			continue
		}
		if !query.Before.IsZero() && doc.CreatedAt.After(query.Before) {
			continue
		}
		if contentSHA256(doc.Content) != digest {
			continue
		}
		vector, err := embedding.Decode(blob, query.Dimensions)
		if err != nil {
			continue
		}
		dot := float64(embedding.Dot(query.QueryVector, vector))
		if dot <= 0 || math.IsNaN(dot) || math.IsInf(dot, 0) {
			continue
		}
		cosine := min(dot, 1.0)
		recency := textsearch.RecencyScore(now, doc.CreatedAt, textsearch.DefaultHalfLifeDays)
		candidate := StoredHit{
			StoredDocument: doc,
			Score:          semanticCosineWeight*cosine + semanticRecencyWeight*recency,
		}
		offerMemoryHit(&top, query.Limit, &candidate)
	}
	if err := rows.Err(); err != nil {
		return VectorSearchResult{}, sqlite.Classify(fmt.Errorf("search memory vectors: %w", err))
	}
	return VectorSearchResult{Hits: top, ScannedRows: scanned}, nil
}

func scanMemoryVectorRow(rows *sql.Rows) (doc StoredDocument, digest string, blob []byte, err error) {
	var createdAtRaw string
	var expiresAtRaw sql.NullString
	var taintedRaw sql.NullInt64
	var promptVersion, modelProtocol, modelName sql.NullString
	if err := rows.Scan(
		&doc.ID, &doc.OwnerID, &doc.SessionID, &doc.Kind,
		&doc.FromSequence, &doc.ToSequence, &doc.Content, &doc.TrustLabel,
		&taintedRaw, &promptVersion, &modelProtocol, &modelName,
		&createdAtRaw, &expiresAtRaw, &doc.AgentID, &digest, &blob,
	); err != nil {
		return StoredDocument{}, "", nil, sqlite.Classify(fmt.Errorf("scan memory vector row: %w", err))
	}
	if err := scanMemoryTainted(taintedRaw, &doc.Tainted); err != nil {
		return StoredDocument{}, "", nil, err
	}
	createdAt, err := sqlite.ParseTime(createdAtRaw)
	if err != nil {
		return StoredDocument{}, "", nil, fault.New(ErrorCodeMemoryInvalid, "memory created_at is not valid")
	}
	doc.CreatedAt = createdAt
	doc.PromptVersion = promptVersion.String
	doc.ModelProtocol = modelProtocol.String
	doc.ModelName = modelName.String
	if expiresAtRaw.Valid {
		expiresAt, err := sqlite.ParseTime(expiresAtRaw.String)
		if err != nil {
			return StoredDocument{}, "", nil, fault.New(ErrorCodeMemoryInvalid, "memory expires_at is not valid")
		}
		doc.ExpiresAt = &expiresAt
	}
	return doc, digest, blob, nil
}

func (s *SQLiteStore) EmbeddingCoverage(ctx context.Context, fingerprint string, includeNonOwner bool, now time.Time) (EmbeddingCoverage, error) {
	if s == nil || s.db == nil {
		return EmbeddingCoverage{}, fault.New(ErrorCodeInvalidArgument, "db must not be nil")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	sqlQuery := `SELECT count(d.id),
		coalesce(sum(CASE WHEN e.fingerprint = ? THEN 1 ELSE 0 END), 0),
		coalesce(sum(CASE WHEN e.document_id IS NOT NULL AND e.fingerprint != ? THEN 1 ELSE 0 END), 0)
		FROM memory_document d
		LEFT JOIN memory_embedding e ON e.document_id = d.id
		WHERE (d.expires_at IS NULL OR d.expires_at > ?)`
	args := []any{fingerprint, fingerprint, sqlite.FormatTime(now.UTC().Add(-time.Second))}
	if !includeNonOwner {
		sqlQuery += ` AND d.owner_id = ?`
		args = append(args, principal.OwnerID)
	}
	var cov EmbeddingCoverage
	if err := sqlite.Conn(ctx, s.db).QueryRowContext(ctx, sqlQuery, args...).Scan(&cov.Total, &cov.Embedded, &cov.Stale); err != nil {
		return EmbeddingCoverage{}, sqlite.Classify(fmt.Errorf("count memory embedding coverage: %w", err))
	}
	return cov, nil
}
