package store

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
)

const (
	KBMaxVectorPage   = 4096
	KBMaxVectorChunks = 200
)

type KBCollectionVersion struct {
	Collection  string
	Generation  int64
	Fingerprint string
}

type KBVectorPage struct {
	RowIDs  []int64
	Digests []string
	Values  []float32
}

type KBVectorChunk struct {
	RowID      int64
	ChunkID    string
	SourceID   string
	Collection string
	Label      string
	Heading    string
	Ordinal    int
	Content    string
	SHA256     string
}

type KBVectorReader interface {
	CollectionVersions(ctx context.Context, collections []string) ([]KBCollectionVersion, error)
	VectorPage(ctx context.Context, collection, fingerprint string, dimensions int, afterRowID int64, limit int) (KBVectorPage, error)
	ChunksByRowID(ctx context.Context, rowIDs []int64) ([]KBVectorChunk, error)
}

const kbCollectionVersionsSQL = `SELECT id, generation, coalesce(embedding_fingerprint, '')
	FROM kb_collection WHERE id IN (SELECT value FROM json_each(?))`

const kbChunksByRowIDSQL = `SELECT k.rowid, k.id, k.source_id, s.collection, s.label, coalesce(k.heading, ''), k.ordinal, k.content, k.content_sha256
	FROM kb_chunk k JOIN kb_source s ON s.id = k.source_id
	WHERE k.rowid IN (SELECT value FROM json_each(?))`

const kbVectorPageSQL = `SELECT e.chunk_rowid, e.content_sha256, e.vector
	FROM kb_embedding e
	CROSS JOIN kb_chunk c ON c.rowid = e.chunk_rowid
	CROSS JOIN kb_source s ON s.id = c.source_id
	WHERE e.chunk_rowid > ? AND s.collection = ? AND e.fingerprint = ? AND e.content_sha256 = c.content_sha256
	ORDER BY e.chunk_rowid LIMIT ?`

type sqliteKBVectorReader struct {
	db *sql.DB
}

func NewKBVectorReader(db *sql.DB) KBVectorReader {
	return &sqliteKBVectorReader{db: db}
}

func (s *sqliteKBVectorReader) CollectionVersions(ctx context.Context, collections []string) ([]KBCollectionVersion, error) {
	if s.db == nil {
		return nil, errNilArgument("db")
	}
	if err := validateKBCollections(collections); err != nil {
		return nil, err
	}
	scope, err := json.Marshal(collections)
	if err != nil {
		return nil, fmt.Errorf("encode knowledge collections: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, kbCollectionVersionsSQL, string(scope))
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("read knowledge collection versions: %w", err))
	}
	defer func() { _ = rows.Close() }()
	versions := make([]KBCollectionVersion, 0, len(collections))
	for rows.Next() {
		var version KBCollectionVersion
		if err := rows.Scan(&version.Collection, &version.Generation, &version.Fingerprint); err != nil {
			return nil, fmt.Errorf("scan knowledge collection version: %w", err)
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		return nil, classifyBusy(fmt.Errorf("read knowledge collection versions: %w", err))
	}
	return versions, nil
}

func (s *sqliteKBVectorReader) VectorPage(ctx context.Context, collection, fingerprint string, dimensions int, afterRowID int64, limit int) (KBVectorPage, error) {
	if s.db == nil {
		return KBVectorPage{}, errNilArgument("db")
	}
	if collection == "" || fingerprint == "" || dimensions <= 0 || limit <= 0 || limit > KBMaxVectorPage {
		return KBVectorPage{}, Errorf(ErrorCodeInvalidArgument, "knowledge vector page needs a collection, a fingerprint, dimensions and a limit up to %d", KBMaxVectorPage)
	}
	rows, err := s.db.QueryContext(ctx, kbVectorPageSQL, afterRowID, collection, fingerprint, limit)
	if err != nil {
		return KBVectorPage{}, classifyBusy(fmt.Errorf("read knowledge vectors: %w", err))
	}
	defer func() { _ = rows.Close() }()
	page := KBVectorPage{Values: make([]float32, 0, limit*dimensions)}
	for rows.Next() {
		var rowID int64
		var digest string
		var blob []byte
		if err := rows.Scan(&rowID, &digest, &blob); err != nil {
			return KBVectorPage{}, fmt.Errorf("scan knowledge vector: %w", err)
		}
		if len(blob) != dimensions*kbVectorValueBytes {
			return KBVectorPage{}, Errorf(ErrorCodeInvalidArgument, "knowledge vector has %d bytes, want %d", len(blob), dimensions*kbVectorValueBytes)
		}
		page.RowIDs = append(page.RowIDs, rowID)
		page.Digests = append(page.Digests, digest)
		for i := 0; i < len(blob); i += kbVectorValueBytes {
			page.Values = append(page.Values, math.Float32frombits(binary.LittleEndian.Uint32(blob[i:])))
		}
	}
	if err := rows.Err(); err != nil {
		return KBVectorPage{}, classifyBusy(fmt.Errorf("read knowledge vectors: %w", err))
	}
	return page, nil
}

func (s *sqliteKBVectorReader) ChunksByRowID(ctx context.Context, rowIDs []int64) ([]KBVectorChunk, error) {
	if s.db == nil {
		return nil, errNilArgument("db")
	}
	if len(rowIDs) == 0 || len(rowIDs) > KBMaxVectorChunks {
		return nil, Errorf(ErrorCodeInvalidArgument, "knowledge chunk lookup needs between 1 and %d rows", KBMaxVectorChunks)
	}
	list, err := json.Marshal(rowIDs)
	if err != nil {
		return nil, fmt.Errorf("encode knowledge chunk rows: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, kbChunksByRowIDSQL, string(list))
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("read knowledge chunks: %w", err))
	}
	defer func() { _ = rows.Close() }()
	chunks := make([]KBVectorChunk, 0, len(rowIDs))
	for rows.Next() {
		var chunk KBVectorChunk
		if err := rows.Scan(&chunk.RowID, &chunk.ChunkID, &chunk.SourceID, &chunk.Collection, &chunk.Label, &chunk.Heading, &chunk.Ordinal, &chunk.Content, &chunk.SHA256); err != nil {
			return nil, fmt.Errorf("scan knowledge chunk: %w", err)
		}
		chunks = append(chunks, chunk)
	}
	if err := rows.Err(); err != nil {
		return nil, classifyBusy(fmt.Errorf("read knowledge chunks: %w", err))
	}
	return chunks, nil
}
