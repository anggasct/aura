package memory

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/embedding"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/secret"
)

const (
	ModeLexical         = "lexical"
	ModeHybrid          = "hybrid"
	ModeLexicalFallback = "lexical_fallback"

	QueryEmbedTimeout  = embedding.QueryEmbedTimeout
	MaxEmbedQueryBytes = embedding.MaxEmbedQueryBytes

	defaultEmbedBatchSize    = 96
	defaultBackfillInterval  = 30 * time.Second
	maxBackfillRowsPerTick   = 200
	maxVectorScanRows        = 10000
	vectorScanCancelInterval = 256
	postWriteEmbedTimeout    = 10 * time.Second

	semanticCosineWeight  = 0.85
	semanticRecencyWeight = 0.15
)

type Embedder interface {
	Embed(ctx context.Context, role embedding.Role, texts []string) ([][]float32, error)
}

type SecretSource interface {
	Snapshot(ctx context.Context) (secret.Screen, error)
}

type EmbeddingSettings struct {
	Fingerprint   string
	Dimensions    int
	BatchSize     int
	PublicQueries bool
	QueryTimeout  time.Duration
}

func (e *EmbeddingSettings) valid() bool {
	return e != nil && e.Fingerprint != "" && e.Dimensions > 0
}

func (e *EmbeddingSettings) batchSize() int {
	if e == nil || e.BatchSize <= 0 {
		return defaultEmbedBatchSize
	}
	return min(e.BatchSize, embedding.MaxBatchSize)
}

func (e *EmbeddingSettings) queryTimeout() time.Duration {
	if e == nil || e.QueryTimeout <= 0 {
		return QueryEmbedTimeout
	}
	return e.QueryTimeout
}

type PendingEmbedding struct {
	DocumentID string
	OwnerID    string
	Content    string
	CreatedAt  string
}

type EmbeddingWrite struct {
	DocumentID    string
	ContentSHA256 string
	Vector        []float32
	CreatedAt     time.Time
}

type EmbeddingCoverage struct {
	Total    int
	Embedded int
	Stale    int
}

type VectorQuery struct {
	OwnerID     string
	AgentIDs    []string
	SessionID   string
	After       time.Time
	Before      time.Time
	Now         time.Time
	Fingerprint string
	Dimensions  int
	QueryVector []float32
	Limit       int
	MaxScanRows int
}

type VectorSearchResult struct {
	Hits        []StoredHit
	ScannedRows int
}

type VectorStore interface {
	UpsertEmbeddings(ctx context.Context, fingerprint string, writes []EmbeddingWrite) error
	PendingEmbeddings(ctx context.Context, fingerprint string, includeNonOwner bool, afterCreatedAt, afterID string, now time.Time, limit int) ([]PendingEmbedding, error)
	HasEmbeddings(ctx context.Context, query *VectorQuery) (bool, error)
	SearchVectors(ctx context.Context, query *VectorQuery) (VectorSearchResult, error)
	EmbeddingCoverage(ctx context.Context, fingerprint string, includeNonOwner bool, now time.Time) (EmbeddingCoverage, error)
}

func contentSHA256(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func (s *Service) EmbeddingActive() bool {
	return s != nil && s.embeddingEnabled && s.embedder != nil && s.embedding.valid() && s.vectors != nil
}

func (s *Service) allowsQueryEmbed(ownerID string, class principal.Class) bool {
	if s.embedding == nil {
		return false
	}
	effective := class
	if effective == "" {
		effective = principal.ClassForPrincipal(ownerID)
	}
	switch effective {
	case principal.ClassOwner, principal.ClassTrusted:
		return true
	case principal.ClassPublic:
		return s.embedding.PublicQueries
	default:
		return false
	}
}

func (s *Service) allowsDocumentEmbed(ownerID string) bool {
	if s.embedding == nil {
		return false
	}
	if ownerID == principal.OwnerID {
		return true
	}
	return s.embedding.PublicQueries
}

func (s *Service) embedScreenSnapshot(ctx context.Context) (secret.Screen, error) {
	if s.secretSource != nil {
		return s.secretSource.Snapshot(ctx)
	}
	return s.secrets, nil
}

func (s *Service) textBlockedForEmbed(text string, screen secret.Screen) bool {
	if _, matched := secret.MatchSensitive(text, screen); matched {
		return true
	}
	if s.secrets != nil && s.secrets.Contains(text) {
		return true
	}
	return false
}

func (s *Service) prepareQueryVector(
	ctx context.Context,
	ownerID string,
	class principal.Class,
	agentIDs []string,
	sessionID string,
	after, before, now time.Time,
	query string,
) (vec []float32, mode string) {
	if !s.EmbeddingActive() {
		return nil, ModeLexical
	}
	if !s.allowsQueryEmbed(ownerID, class) {
		return nil, ModeLexical
	}
	trimmed := strings.TrimSpace(query)
	if trimmed == "" || len(trimmed) > MaxEmbedQueryBytes {
		return nil, ModeLexical
	}
	has, err := s.vectors.HasEmbeddings(ctx, &VectorQuery{
		OwnerID:     ownerID,
		AgentIDs:    agentIDs,
		SessionID:   sessionID,
		After:       after,
		Before:      before,
		Now:         now,
		Fingerprint: s.embedding.Fingerprint,
	})
	if err != nil || !has {
		return nil, ModeLexical
	}
	screen, err := s.embedScreenSnapshot(ctx)
	if err != nil || s.textBlockedForEmbed(trimmed, screen) {
		return nil, ModeLexical
	}
	embedCtx, cancel := context.WithTimeout(ctx, s.embedding.queryTimeout())
	defer cancel()
	vectors, err := s.embedder.Embed(embedCtx, embedding.RoleQuery, []string{trimmed})
	if err != nil || len(vectors) != 1 {
		return nil, ModeLexicalFallback
	}
	unit, ok := embedding.Normalize(vectors[0], s.embedding.Dimensions)
	if !ok {
		return nil, ModeLexicalFallback
	}
	return unit, ModeHybrid
}

func (s *Service) searchHybrid(ctx context.Context, query *StoredQuery, queryVec []float32, mode string) ([]StoredHit, string, error) {
	if mode != ModeHybrid || len(queryVec) == 0 || !s.EmbeddingActive() {
		hits, err := s.documents.Search(ctx, query)
		return hits, mode, err
	}
	candidateLimit := max(query.Limit, min(max(query.Limit*4, minSearchCandidateRows), maxSearchCandidateRows))
	lexQuery := *query
	lexQuery.Limit = candidateLimit
	lexical, err := s.documents.Search(ctx, &lexQuery)
	if err != nil {
		return nil, mode, err
	}
	vecResult, err := s.vectors.SearchVectors(ctx, &VectorQuery{
		OwnerID:     query.OwnerID,
		AgentIDs:    query.AgentIDs,
		SessionID:   query.SessionID,
		After:       query.After,
		Before:      query.Before,
		Now:         query.Now,
		Fingerprint: s.embedding.Fingerprint,
		Dimensions:  s.embedding.Dimensions,
		QueryVector: queryVec,
		Limit:       candidateLimit,
		MaxScanRows: s.maxVectorScanRows,
	})
	if err != nil {
		if len(lexical) > query.Limit {
			lexical = lexical[:query.Limit]
		}
		return lexical, ModeLexicalFallback, nil
	}
	fused := fuseMemoryHits(lexical, vecResult.Hits, candidateLimit)
	if len(fused) > query.Limit {
		fused = fused[:query.Limit]
	}
	return fused, ModeHybrid, nil
}

func fuseMemoryHits(lexical, semantic []StoredHit, depth int) []StoredHit {
	byID := make(map[string]StoredHit, len(lexical)+len(semantic))
	lexIDs := make([]string, len(lexical))
	for i := range lexical {
		lexIDs[i] = lexical[i].ID
		if _, exists := byID[lexical[i].ID]; !exists {
			byID[lexical[i].ID] = lexical[i]
		}
	}
	semIDs := make([]string, len(semantic))
	for i := range semantic {
		semIDs[i] = semantic[i].ID
		if _, exists := byID[semantic[i].ID]; !exists {
			byID[semantic[i].ID] = semantic[i]
		}
	}
	items := embedding.FuseRRF(lexIDs, semIDs, depth, cmp.Compare[string])
	out := make([]StoredHit, len(items))
	for i, item := range items {
		hit := byID[item.Key]
		hit.Score = item.Score
		out[i] = hit
	}
	return out
}

func (s *Service) embedDocumentBestEffort(ctx context.Context, documentID, ownerID, content string) {
	if !s.EmbeddingActive() || !s.allowsDocumentEmbed(ownerID) || strings.TrimSpace(content) == "" {
		return
	}
	screen, err := s.embedScreenSnapshot(ctx)
	if err != nil || s.textBlockedForEmbed(content, screen) {
		return
	}
	embedCtx, cancel := context.WithTimeout(ctx, postWriteEmbedTimeout)
	defer cancel()
	vectors, err := s.embedder.Embed(embedCtx, embedding.RoleDocument, []string{content})
	if err != nil || len(vectors) != 1 {
		return
	}
	unit, ok := embedding.Normalize(vectors[0], s.embedding.Dimensions)
	if !ok {
		return
	}
	_ = s.vectors.UpsertEmbeddings(ctx, s.embedding.Fingerprint, []EmbeddingWrite{{
		DocumentID:    documentID,
		ContentSHA256: contentSHA256(content),
		Vector:        unit,
		CreatedAt:     time.Now().UTC(),
	}})
}

func (s *Service) BackfillEmbeddingsOnce(ctx context.Context, maxRows int) (int, error) {
	if !s.EmbeddingActive() {
		return 0, nil
	}
	if maxRows <= 0 {
		maxRows = maxBackfillRowsPerTick
	}
	batchSize := min(s.embedding.batchSize(), maxRows)
	var cursorCreatedAt, cursorID string
	embedded := 0
	scanned := 0
	now := time.Now().UTC()
	for scanned < maxRows {
		if err := ctx.Err(); err != nil {
			return embedded, err
		}
		remaining := min(batchSize, maxRows-scanned)
		pending, err := s.vectors.PendingEmbeddings(ctx, s.embedding.Fingerprint, s.embedding.PublicQueries, cursorCreatedAt, cursorID, now, remaining)
		if err != nil {
			return embedded, err
		}
		if len(pending) == 0 {
			return embedded, nil
		}
		last := pending[len(pending)-1]
		cursorCreatedAt, cursorID = last.CreatedAt, last.DocumentID
		scanned += len(pending)

		screen, err := s.embedScreenSnapshot(ctx)
		if err != nil {
			return embedded, err
		}
		eligible := make([]PendingEmbedding, 0, len(pending))
		texts := make([]string, 0, len(pending))
		for i := range pending {
			row := &pending[i]
			if !s.allowsDocumentEmbed(row.OwnerID) || strings.TrimSpace(row.Content) == "" {
				continue
			}
			if s.textBlockedForEmbed(row.Content, screen) {
				continue
			}
			eligible = append(eligible, *row)
			texts = append(texts, row.Content)
		}
		if len(eligible) == 0 {
			continue
		}
		vectors, err := s.embedder.Embed(ctx, embedding.RoleDocument, texts)
		if err != nil {
			return embedded, err
		}
		if len(vectors) != len(eligible) {
			return embedded, nil
		}
		writes := make([]EmbeddingWrite, 0, len(eligible))
		writeAt := time.Now().UTC()
		for i := range eligible {
			unit, ok := embedding.Normalize(vectors[i], s.embedding.Dimensions)
			if !ok {
				continue
			}
			writes = append(writes, EmbeddingWrite{
				DocumentID:    eligible[i].DocumentID,
				ContentSHA256: contentSHA256(eligible[i].Content),
				Vector:        unit,
				CreatedAt:     writeAt,
			})
		}
		if len(writes) == 0 {
			continue
		}
		if err := s.vectors.UpsertEmbeddings(ctx, s.embedding.Fingerprint, writes); err != nil {
			return embedded, err
		}
		embedded += len(writes)
	}
	return embedded, nil
}

func (s *Service) RunEmbeddingBackfill(ctx context.Context, interval time.Duration) {
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
