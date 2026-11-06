package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

const (
	KBMaxSearchCollections = 64
	KBMaxSearchTerms       = 16
	KBMaxSearchDepth       = 200
)

type KBHit struct {
	ChunkID    string
	SourceID   string
	Collection string
	Label      string
	Heading    string
	Ordinal    int
	Content    string
	Score      float64
}

type KBIndex interface {
	ScopeChunks(ctx context.Context, collections []string) (int, error)
	TermFrequency(ctx context.Context, collections []string, term string, limit int) (int, error)
	Search(ctx context.Context, collections []string, terms []string, depth int) ([]KBHit, error)
	ScopeSizes(ctx context.Context) (map[string]int, error)
}

type sqliteKBIndex struct {
	db *sql.DB
}

func NewKBIndex(db *sql.DB) KBIndex {
	return &sqliteKBIndex{db: db}
}

func validateKBCollections(collections []string) error {
	if len(collections) == 0 || len(collections) > KBMaxSearchCollections {
		return Errorf(ErrorCodeInvalidArgument, "knowledge search needs between 1 and %d collections", KBMaxSearchCollections)
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
		scopes = append(scopes, quoteFTSTerm(KBScope(collection)))
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

func (s *sqliteKBIndex) ScopeChunks(ctx context.Context, collections []string) (int, error) {
	if s.db == nil {
		return 0, errNilArgument("db")
	}
	if err := validateKBCollections(collections); err != nil {
		return 0, err
	}
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM kb_source s JOIN kb_chunk c ON c.source_id = s.id WHERE s.collection IN (`+placeholders(len(collections))+`)`,
		stringArgs(collections)...).Scan(&count)
	if err != nil {
		return 0, classifyBusy(fmt.Errorf("count knowledge scope: %w", err))
	}
	return count, nil
}

func (s *sqliteKBIndex) TermFrequency(ctx context.Context, collections []string, term string, limit int) (int, error) {
	if s.db == nil {
		return 0, errNilArgument("db")
	}
	if err := validateKBCollections(collections); err != nil {
		return 0, err
	}
	if term == "" || limit <= 0 {
		return 0, Errorf(ErrorCodeInvalidArgument, "knowledge term and limit are required")
	}
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM (SELECT 1 FROM kb_chunk_fts WHERE kb_chunk_fts MATCH ? LIMIT ?)`,
		kbScopeFilter(collections)+" AND {content heading}:"+quoteFTSTerm(term), limit).Scan(&count)
	if err != nil {
		return 0, classifyBusy(fmt.Errorf("count knowledge term: %w", err))
	}
	return count, nil
}

func (s *sqliteKBIndex) Search(ctx context.Context, collections, terms []string, depth int) ([]KBHit, error) {
	if s.db == nil {
		return nil, errNilArgument("db")
	}
	if err := validateKBCollections(collections); err != nil {
		return nil, err
	}
	if len(terms) == 0 || len(terms) > KBMaxSearchTerms || depth <= 0 || depth > KBMaxSearchDepth {
		return nil, Errorf(ErrorCodeInvalidArgument, "knowledge search needs 1 to %d terms and a depth up to %d", KBMaxSearchTerms, KBMaxSearchDepth)
	}
	rows, err := s.db.QueryContext(ctx,
		`WITH ranked AS (
		   SELECT rowid AS rid, bm25(kb_chunk_fts, 1.0, 1.0, 0.0) AS score
		   FROM kb_chunk_fts WHERE kb_chunk_fts MATCH ? ORDER BY score LIMIT ?
		 )
		 SELECT k.id, k.source_id, s.collection, s.label, coalesce(k.heading, ''), k.ordinal, k.content, r.score
		 FROM ranked r
		 JOIN kb_chunk k ON k.rowid = r.rid
		 JOIN kb_source s ON s.id = k.source_id
		 ORDER BY r.score, k.id`, kbMatch(collections, terms), depth)
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("search knowledge: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var hits []KBHit
	for rows.Next() {
		var hit KBHit
		if err := rows.Scan(&hit.ChunkID, &hit.SourceID, &hit.Collection, &hit.Label, &hit.Heading, &hit.Ordinal, &hit.Content, &hit.Score); err != nil {
			return nil, fmt.Errorf("scan knowledge hit: %w", err)
		}
		if slices.Contains(collections, hit.Collection) {
			hits = append(hits, hit)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, classifyBusy(fmt.Errorf("search knowledge: %w", err))
	}
	return hits, nil
}

func (s *sqliteKBIndex) ScopeSizes(ctx context.Context) (map[string]int, error) {
	if s.db == nil {
		return nil, errNilArgument("db")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT s.collection, count(c.rowid) FROM kb_source s JOIN kb_chunk c ON c.source_id = s.id GROUP BY s.collection`)
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("size knowledge scopes: %w", err))
	}
	defer func() { _ = rows.Close() }()
	sizes := map[string]int{}
	for rows.Next() {
		var collection string
		var count int
		if err := rows.Scan(&collection, &count); err != nil {
			return nil, fmt.Errorf("scan knowledge scope size: %w", err)
		}
		sizes[collection] = count
	}
	if err := rows.Err(); err != nil {
		return nil, classifyBusy(fmt.Errorf("size knowledge scopes: %w", err))
	}
	return sizes, nil
}
