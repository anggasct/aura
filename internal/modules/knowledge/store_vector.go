package knowledge

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const (
	MaxVectorPage   = 4096
	MaxVectorChunks = 200
)

const kbCollectionVersionsSQL = `SELECT id, generation, coalesce(embedding_fingerprint, '')
	FROM kb_collection WHERE id IN (SELECT value FROM json_each(?))`

const kbChunksByRowIDSQL = `SELECT k.rowid, k.id, k.source_id, s.collection, s.label, coalesce(k.heading, ''), k.ordinal, k.content, k.content_sha256,
	(c.trust = 'trusted' AND s.vouched_sha256 IS NOT NULL AND s.vouched_sha256 <> '' AND s.vouched_sha256 = s.content_sha256)
	FROM kb_chunk k
	JOIN kb_source s ON s.id = k.source_id
	JOIN kb_collection c ON c.id = s.collection
	WHERE k.rowid IN (SELECT value FROM json_each(?))`

const kbVectorPageSQL = `SELECT e.chunk_rowid, e.content_sha256, e.vector
	FROM kb_embedding e
	CROSS JOIN kb_chunk c ON c.rowid = e.chunk_rowid
	CROSS JOIN kb_source s ON s.id = c.source_id
	WHERE e.chunk_rowid > ? AND s.collection = ? AND e.fingerprint = ? AND e.content_sha256 = c.content_sha256
	ORDER BY e.chunk_rowid LIMIT ?`

type SQLiteVectorSource struct {
	db    *sql.DB
	store *SQLiteStore
}

var (
	_ VectorReader = (*SQLiteVectorSource)(nil)
	_ VectorLoader = (*SQLiteVectorSource)(nil)
)

func NewSQLiteVectorSource(db *sql.DB) (*SQLiteVectorSource, error) {
	if db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	return &SQLiteVectorSource{db: db, store: &SQLiteStore{db: db}}, nil
}

func (s *SQLiteVectorSource) EmbeddingState(ctx context.Context, collection, fingerprint string) (EmbeddingState, error) {
	if s == nil || s.store == nil {
		return EmbeddingState{}, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	return s.store.EmbeddingState(ctx, collection, fingerprint)
}

func (s *SQLiteVectorSource) Versions(ctx context.Context, collections []string) ([]CollectionVersion, error) {
	if s == nil || s.db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if err := validateSearchCollections(collections); err != nil {
		return nil, err
	}
	scope, err := json.Marshal(collections)
	if err != nil {
		return nil, fmt.Errorf("encode knowledge collections: %w", err)
	}
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx, kbCollectionVersionsSQL, string(scope))
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("read knowledge collection versions: %w", err))
	}
	defer func() { _ = rows.Close() }()
	versions := make([]CollectionVersion, 0, len(collections))
	for rows.Next() {
		var version CollectionVersion
		if err := rows.Scan(&version.Collection, &version.Generation, &version.Fingerprint); err != nil {
			return nil, sqlite.Classify(fmt.Errorf("scan knowledge collection version: %w", err))
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("read knowledge collection versions: %w", err))
	}
	return versions, nil
}

func (s *SQLiteVectorSource) Page(ctx context.Context, collection, fingerprint string, dimensions int, afterRowID int64, limit int) (VectorPage, error) {
	if s == nil || s.db == nil {
		return VectorPage{}, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if collection == "" || fingerprint == "" || dimensions <= 0 || limit <= 0 || limit > MaxVectorPage {
		return VectorPage{}, Errorf(ErrorCodeInvalidArgument, "knowledge vector page needs a collection, a fingerprint, dimensions and a limit up to %d", MaxVectorPage)
	}
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx, kbVectorPageSQL, afterRowID, collection, fingerprint, limit)
	if err != nil {
		return VectorPage{}, sqlite.Classify(fmt.Errorf("read knowledge vectors: %w", err))
	}
	defer func() { _ = rows.Close() }()
	page := VectorPage{Values: make([]float32, 0, limit*dimensions)}
	for rows.Next() {
		var rowID int64
		var digest string
		var blob []byte
		if err := rows.Scan(&rowID, &digest, &blob); err != nil {
			return VectorPage{}, sqlite.Classify(fmt.Errorf("scan knowledge vector: %w", err))
		}
		if len(blob) != dimensions*kbVectorValueBytes {
			return VectorPage{}, Errorf(ErrorCodeInvalidArgument, "knowledge vector has %d bytes, want %d", len(blob), dimensions*kbVectorValueBytes)
		}
		page.RowIDs = append(page.RowIDs, rowID)
		page.Digests = append(page.Digests, digest)
		for i := 0; i < len(blob); i += kbVectorValueBytes {
			page.Values = append(page.Values, math.Float32frombits(binary.LittleEndian.Uint32(blob[i:])))
		}
	}
	if err := rows.Err(); err != nil {
		return VectorPage{}, sqlite.Classify(fmt.Errorf("read knowledge vectors: %w", err))
	}
	return page, nil
}

func (s *SQLiteVectorSource) Chunks(ctx context.Context, rowIDs []int64) ([]VectorChunk, error) {
	if s == nil || s.db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if len(rowIDs) == 0 || len(rowIDs) > MaxVectorChunks {
		return nil, Errorf(ErrorCodeInvalidArgument, "knowledge chunk lookup needs between 1 and %d rows", MaxVectorChunks)
	}
	list, err := json.Marshal(rowIDs)
	if err != nil {
		return nil, fmt.Errorf("encode knowledge chunk rows: %w", err)
	}
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx, kbChunksByRowIDSQL, string(list))
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("read knowledge chunks: %w", err))
	}
	defer func() { _ = rows.Close() }()
	chunks := make([]VectorChunk, 0, len(rowIDs))
	for rows.Next() {
		var chunk VectorChunk
		var vouched int
		if err := rows.Scan(
			&chunk.RowID, &chunk.Hit.ChunkID, &chunk.Hit.SourceID, &chunk.Hit.Collection,
			&chunk.Hit.Label, &chunk.Hit.Heading, &chunk.Hit.Ordinal, &chunk.Hit.Content, &chunk.SHA256, &vouched,
		); err != nil {
			return nil, sqlite.Classify(fmt.Errorf("scan knowledge chunk: %w", err))
		}
		chunk.Hit.Trusted = vouched == 1
		chunks = append(chunks, chunk)
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("read knowledge chunks: %w", err))
	}
	return chunks, nil
}
