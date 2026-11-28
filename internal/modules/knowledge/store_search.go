package knowledge

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const (
	MaxSearchCollections = 64
	MaxSearchTerms       = 16
	MaxSearchDepth       = 200
)

type SQLiteIndex struct {
	db *sql.DB
}

var _ Index = (*SQLiteIndex)(nil)

func NewSQLiteIndex(db *sql.DB) (*SQLiteIndex, error) {
	if db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	return &SQLiteIndex{db: db}, nil
}

func validateSearchCollections(collections []string) error {
	if len(collections) == 0 || len(collections) > MaxSearchCollections {
		return Errorf(ErrorCodeInvalidArgument, "knowledge search needs between 1 and %d collections", MaxSearchCollections)
	}
	for _, collection := range collections {
		if collection == "" {
			return Errorf(ErrorCodeInvalidArgument, "knowledge collection must not be empty")
		}
	}
	return nil
}

func quoteFTSTerm(term string) string {
	return `"` + strings.ReplaceAll(term, `"`, `""`) + `"`
}

func kbScopeFilter(collections []string) string {
	scopes := make([]string, 0, len(collections))
	for _, collection := range collections {
		scopes = append(scopes, quoteFTSTerm(ScopeToken(collection)))
	}
	return "scope:(" + strings.Join(scopes, " OR ") + ")"
}

func kbMatch(collections, terms []string) string {
	quoted := make([]string, 0, len(terms))
	for _, term := range terms {
		quoted = append(quoted, quoteFTSTerm(term))
	}
	return kbScopeFilter(collections) + " AND {content heading}:(" + strings.Join(quoted, " OR ") + ")"
}

func placeholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

func stringArgs(values []string) []any {
	args := make([]any, 0, len(values))
	for _, value := range values {
		args = append(args, value)
	}
	return args
}

func (s *SQLiteIndex) ScopeChunks(ctx context.Context, collections []string) (int, error) {
	if s == nil || s.db == nil {
		return 0, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if err := validateSearchCollections(collections); err != nil {
		return 0, err
	}
	var count int
	err := sqlite.Conn(ctx, s.db).QueryRowContext(ctx,
		`SELECT count(*) FROM kb_source s JOIN kb_chunk c ON c.source_id = s.id WHERE s.collection IN (`+placeholders(len(collections))+`)`,
		stringArgs(collections)...).Scan(&count)
	if err != nil {
		return 0, sqlite.Classify(fmt.Errorf("count knowledge scope: %w", err))
	}
	return count, nil
}

func (s *SQLiteIndex) TermFrequency(ctx context.Context, collections []string, term string, limit int) (int, error) {
	if s == nil || s.db == nil {
		return 0, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if err := validateSearchCollections(collections); err != nil {
		return 0, err
	}
	if term == "" || limit <= 0 {
		return 0, Errorf(ErrorCodeInvalidArgument, "knowledge term and limit are required")
	}
	var count int
	err := sqlite.Conn(ctx, s.db).QueryRowContext(ctx,
		`SELECT count(*) FROM (SELECT 1 FROM kb_chunk_fts WHERE kb_chunk_fts MATCH ? LIMIT ?)`,
		kbScopeFilter(collections)+" AND {content heading}:"+quoteFTSTerm(term), limit).Scan(&count)
	if err != nil {
		return 0, sqlite.Classify(fmt.Errorf("count knowledge term: %w", err))
	}
	return count, nil
}

func (s *SQLiteIndex) Search(ctx context.Context, collections, terms []string, depth int) ([]Hit, error) {
	if s == nil || s.db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if err := validateSearchCollections(collections); err != nil {
		return nil, err
	}
	if len(terms) == 0 || len(terms) > MaxSearchTerms || depth <= 0 || depth > MaxSearchDepth {
		return nil, Errorf(ErrorCodeInvalidArgument, "knowledge search needs 1 to %d terms and a depth up to %d", MaxSearchTerms, MaxSearchDepth)
	}
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx,
		`WITH ranked AS (
		   SELECT rowid AS rid, bm25(kb_chunk_fts, 1.0, 1.0, 0.0) AS score
		   FROM kb_chunk_fts WHERE kb_chunk_fts MATCH ? ORDER BY score LIMIT ?
		 )
		 SELECT k.id, k.source_id, s.collection, s.label, coalesce(k.heading, ''), k.ordinal, k.content, r.score,
		        (c.trust = 'trusted' AND s.vouched_sha256 IS NOT NULL AND s.vouched_sha256 <> '' AND s.vouched_sha256 = s.content_sha256)
		 FROM ranked r
		 JOIN kb_chunk k ON k.rowid = r.rid
		 JOIN kb_source s ON s.id = k.source_id
		 JOIN kb_collection c ON c.id = s.collection
		 ORDER BY r.score, k.id`, kbMatch(collections, terms), depth)
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("search knowledge: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var hits []Hit
	for rows.Next() {
		var hit Hit
		var vouched int
		if err := rows.Scan(&hit.ChunkID, &hit.SourceID, &hit.Collection, &hit.Label, &hit.Heading, &hit.Ordinal, &hit.Content, &hit.Score, &vouched); err != nil {
			return nil, sqlite.Classify(fmt.Errorf("scan knowledge hit: %w", err))
		}
		hit.Trusted = vouched == 1
		if slices.Contains(collections, hit.Collection) {
			hits = append(hits, hit)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("search knowledge: %w", err))
	}
	return hits, nil
}

func IndexPresent(ctx context.Context, db *sql.DB) (bool, error) {
	if db == nil {
		return false, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	var tables int
	err := sqlite.Conn(ctx, db).QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE name IN ('kb_collection', 'kb_source', 'kb_chunk', 'kb_chunk_fts')`).Scan(&tables)
	if err != nil {
		return false, sqlite.Classify(fmt.Errorf("probe knowledge tables: %w", err))
	}
	return tables == 4, nil
}
