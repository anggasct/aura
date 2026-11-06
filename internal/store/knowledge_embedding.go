package store

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

const (
	kbVectorValueBytes = 4
	kbUnitTolerance    = 1e-3
)

type KBEmbeddingState struct {
	Exists            bool
	Fingerprint       string
	Chunks            int
	Covered           int
	PendingCharacters int64
}

type KBPendingChunk struct {
	RowID   int64
	Heading string
	Content string
	SHA256  string
}

type KBVectorWrite struct {
	RowID         int64
	ContentSHA256 string
	Vector        []float32
}

type KBCommitResult struct {
	Written   int
	Completed bool
}

type KBDisableResult struct {
	OptedIn      bool
	Vectors      int
	Applied      bool
	WALTruncated bool
}

type KBEmbeddingCoverage struct {
	Collection  string
	Fingerprint string
	Chunks      int
	Covered     int
}

type KBEmbeddingStore interface {
	EmbeddingState(ctx context.Context, collection, fingerprint string) (KBEmbeddingState, error)
	StartEmbedding(ctx context.Context, collection, fingerprint string) error
	PendingChunks(ctx context.Context, collection, fingerprint string, afterRowID int64, limit int) ([]KBPendingChunk, error)
	CommitVectors(ctx context.Context, collection, fingerprint string, vectors []KBVectorWrite) (KBCommitResult, error)
	DisableEmbedding(ctx context.Context, collection string, apply bool) (KBDisableResult, error)
	EmbeddingCoverage(ctx context.Context, fingerprint string) ([]KBEmbeddingCoverage, error)
}

func NewKBEmbeddingStore(db *sql.DB) KBEmbeddingStore {
	return &sqliteKBStore{db: db}
}

const kbPendingJoin = `FROM kb_chunk c
	JOIN kb_source s ON s.id = c.source_id
	LEFT JOIN kb_embedding e ON e.chunk_rowid = c.rowid AND e.fingerprint = ? AND e.content_sha256 = c.content_sha256
	WHERE s.collection = ?`

func encodeKBVector(vector []float32) []byte {
	blob := make([]byte, len(vector)*kbVectorValueBytes)
	for i, value := range vector {
		binary.LittleEndian.PutUint32(blob[i*kbVectorValueBytes:], math.Float32bits(value))
	}
	return blob
}

func validateKBVector(vector []float32) error {
	if len(vector) == 0 {
		return Errorf(ErrorCodeInvalidArgument, "knowledge vector must not be empty")
	}
	var sum float64
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return Errorf(ErrorCodeInvalidArgument, "knowledge vector must be finite")
		}
		sum += float64(value) * float64(value)
	}
	if math.Abs(math.Sqrt(sum)-1) > kbUnitTolerance {
		return Errorf(ErrorCodeInvalidArgument, "knowledge vector must have unit length")
	}
	return nil
}

func (s *sqliteKBStore) EmbeddingState(ctx context.Context, collection, fingerprint string) (KBEmbeddingState, error) {
	if s.db == nil {
		return KBEmbeddingState{}, errNilArgument("db")
	}
	var stored sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT embedding_fingerprint FROM kb_collection WHERE id = ?`, collection).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return KBEmbeddingState{}, nil
	}
	if err != nil {
		return KBEmbeddingState{}, classifyBusy(fmt.Errorf("read knowledge embedding state: %w", err))
	}
	state := KBEmbeddingState{Exists: true, Fingerprint: stored.String}
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*), count(e.chunk_rowid), coalesce(sum(CASE WHEN e.chunk_rowid IS NULL THEN length(coalesce(c.heading, '')) + length(c.content) ELSE 0 END), 0) `+kbPendingJoin,
		fingerprint, collection).Scan(&state.Chunks, &state.Covered, &state.PendingCharacters); err != nil {
		return KBEmbeddingState{}, classifyBusy(fmt.Errorf("count knowledge embedding coverage: %w", err))
	}
	return state, nil
}

func (s *sqliteKBStore) StartEmbedding(ctx context.Context, collection, fingerprint string) error {
	if s.db == nil {
		return errNilArgument("db")
	}
	if fingerprint == "" {
		return Errorf(ErrorCodeInvalidArgument, "embedding fingerprint must not be empty")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE kb_collection SET embedding_fingerprint = ? WHERE id = ?`, fingerprint, collection)
	if err != nil {
		return classifyBusy(fmt.Errorf("record knowledge embedding opt-in: %w", err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count knowledge embedding opt-in: %w", err)
	}
	if affected != 1 {
		return Errorf(ErrorCodeKBSourceNotFound, "knowledge collection does not exist")
	}
	return nil
}

func (s *sqliteKBStore) PendingChunks(ctx context.Context, collection, fingerprint string, afterRowID int64, limit int) ([]KBPendingChunk, error) {
	if s.db == nil {
		return nil, errNilArgument("db")
	}
	if limit <= 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "pending chunk limit must be positive")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.rowid, coalesce(c.heading, ''), c.content, c.content_sha256 `+kbPendingJoin+` AND e.chunk_rowid IS NULL AND c.rowid > ? ORDER BY c.rowid LIMIT ?`,
		fingerprint, collection, afterRowID, limit)
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("list pending knowledge chunks: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var pending []KBPendingChunk
	for rows.Next() {
		var chunk KBPendingChunk
		if err := rows.Scan(&chunk.RowID, &chunk.Heading, &chunk.Content, &chunk.SHA256); err != nil {
			return nil, fmt.Errorf("scan pending knowledge chunk: %w", err)
		}
		pending = append(pending, chunk)
	}
	if err := rows.Err(); err != nil {
		return nil, classifyBusy(fmt.Errorf("list pending knowledge chunks: %w", err))
	}
	return pending, nil
}

const kbCommitVectorSQL = `INSERT INTO kb_embedding (chunk_rowid, fingerprint, content_sha256, vector)
	SELECT c.rowid, ?, c.content_sha256, ?
	FROM kb_chunk c
	JOIN kb_source s ON s.id = c.source_id
	JOIN kb_collection k ON k.id = s.collection
	WHERE c.rowid = ? AND c.content_sha256 = ? AND k.id = ? AND k.embedding_fingerprint = ?
	ON CONFLICT(chunk_rowid) DO UPDATE SET
		fingerprint = excluded.fingerprint, content_sha256 = excluded.content_sha256, vector = excluded.vector`

func (s *sqliteKBStore) CommitVectors(ctx context.Context, collection, fingerprint string, vectors []KBVectorWrite) (KBCommitResult, error) {
	if s.db == nil {
		return KBCommitResult{}, errNilArgument("db")
	}
	if fingerprint == "" || len(vectors) == 0 {
		return KBCommitResult{}, Errorf(ErrorCodeInvalidArgument, "embedding fingerprint and vectors are required")
	}
	for i := range vectors {
		if len(vectors[i].ContentSHA256) != kbSHA256HexLen {
			return KBCommitResult{}, Errorf(ErrorCodeInvalidArgument, "knowledge vector needs the digest of the chunk it was computed from")
		}
		if err := validateKBVector(vectors[i].Vector); err != nil {
			return KBCommitResult{}, err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return KBCommitResult{}, classifyBusy(err)
	}
	defer func() { _ = tx.Rollback() }()
	for i := range vectors {
		result, err := tx.ExecContext(ctx, kbCommitVectorSQL,
			fingerprint, encodeKBVector(vectors[i].Vector), vectors[i].RowID, vectors[i].ContentSHA256, collection, fingerprint)
		if err != nil {
			return KBCommitResult{}, classifyBusy(fmt.Errorf("write knowledge vector: %w", err))
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return KBCommitResult{}, fmt.Errorf("count knowledge vector write: %w", err)
		}
		if affected != 1 {
			return KBCommitResult{}, Errorf(ErrorCodeKBEmbeddingStale, "knowledge chunk or opt-in changed while its vector was computed")
		}
	}
	completed, err := completeKBEmbedding(ctx, tx, collection, fingerprint)
	if err != nil {
		return KBCommitResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return KBCommitResult{}, classifyBusy(err)
	}
	return KBCommitResult{Written: len(vectors), Completed: completed}, nil
}

func completeKBEmbedding(ctx context.Context, tx *sql.Tx, collection, fingerprint string) (bool, error) {
	var remaining bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 `+kbPendingJoin+` AND e.chunk_rowid IS NULL)`, fingerprint, collection).Scan(&remaining); err != nil {
		return false, classifyBusy(fmt.Errorf("check knowledge embedding coverage: %w", err))
	}
	if remaining {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE kb_collection SET generation = generation + 1 WHERE id = ? AND embedding_fingerprint = ?`, collection, fingerprint); err != nil {
		return false, classifyBusy(fmt.Errorf("advance knowledge collection generation: %w", err))
	}
	return true, nil
}

func (s *sqliteKBStore) DisableEmbedding(ctx context.Context, collection string, apply bool) (KBDisableResult, error) {
	if s.db == nil {
		return KBDisableResult{}, errNilArgument("db")
	}
	if !apply {
		return s.previewDisableEmbedding(ctx, collection)
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return KBDisableResult{}, classifyBusy(fmt.Errorf("open knowledge connection: %w", err))
	}
	defer func() { _ = conn.Close() }()
	restore, err := enableSecureDelete(ctx, conn)
	if err != nil {
		return KBDisableResult{}, err
	}
	defer restore()
	result, err := deleteKBVectors(ctx, conn, collection)
	if err != nil {
		return KBDisableResult{}, err
	}
	result.Applied = true
	truncated, err := truncateWAL(ctx, conn)
	if err != nil {
		return result, err
	}
	result.WALTruncated = truncated
	return result, nil
}

const kbCollectionVectorsSQL = `FROM kb_embedding WHERE chunk_rowid IN (
	SELECT c.rowid FROM kb_chunk c JOIN kb_source s ON s.id = c.source_id WHERE s.collection = ?)`

func (s *sqliteKBStore) previewDisableEmbedding(ctx context.Context, collection string) (KBDisableResult, error) {
	var stored sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT embedding_fingerprint FROM kb_collection WHERE id = ?`, collection).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return KBDisableResult{}, Errorf(ErrorCodeKBSourceNotFound, "knowledge collection does not exist")
	}
	if err != nil {
		return KBDisableResult{}, classifyBusy(fmt.Errorf("read knowledge embedding state: %w", err))
	}
	result := KBDisableResult{OptedIn: stored.Valid}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) `+kbCollectionVectorsSQL, collection).Scan(&result.Vectors); err != nil {
		return KBDisableResult{}, classifyBusy(fmt.Errorf("count knowledge vectors: %w", err))
	}
	return result, nil
}

func deleteKBVectors(ctx context.Context, conn *sql.Conn, collection string) (KBDisableResult, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return KBDisableResult{}, classifyBusy(err)
	}
	defer func() { _ = tx.Rollback() }()
	var stored sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT embedding_fingerprint FROM kb_collection WHERE id = ?`, collection).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return KBDisableResult{}, Errorf(ErrorCodeKBSourceNotFound, "knowledge collection does not exist")
	}
	if err != nil {
		return KBDisableResult{}, classifyBusy(fmt.Errorf("read knowledge embedding state: %w", err))
	}
	deleted, err := tx.ExecContext(ctx, `DELETE `+kbCollectionVectorsSQL, collection)
	if err != nil {
		return KBDisableResult{}, classifyBusy(fmt.Errorf("delete knowledge vectors: %w", err))
	}
	vectors, err := deleted.RowsAffected()
	if err != nil {
		return KBDisableResult{}, fmt.Errorf("count deleted knowledge vectors: %w", err)
	}
	if stored.Valid || vectors > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE kb_collection SET embedding_fingerprint = NULL, generation = generation + 1 WHERE id = ?`, collection); err != nil {
			return KBDisableResult{}, classifyBusy(fmt.Errorf("clear knowledge embedding opt-in: %w", err))
		}
	}
	if err := tx.Commit(); err != nil {
		return KBDisableResult{}, classifyBusy(err)
	}
	return KBDisableResult{OptedIn: stored.Valid, Vectors: int(vectors)}, nil
}

func (s *sqliteKBStore) EmbeddingCoverage(ctx context.Context, fingerprint string) ([]KBEmbeddingCoverage, error) {
	if s.db == nil {
		return nil, errNilArgument("db")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT k.id, k.embedding_fingerprint, count(c.rowid), count(e.chunk_rowid)
		 FROM kb_collection k
		 LEFT JOIN kb_source s ON s.collection = k.id
		 LEFT JOIN kb_chunk c ON c.source_id = s.id
		 LEFT JOIN kb_embedding e ON e.chunk_rowid = c.rowid AND e.fingerprint = ? AND e.content_sha256 = c.content_sha256
		 WHERE k.embedding_fingerprint IS NOT NULL
		 GROUP BY k.id, k.embedding_fingerprint
		 ORDER BY k.id`, fingerprint)
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("list knowledge embedding coverage: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var out []KBEmbeddingCoverage
	for rows.Next() {
		var coverage KBEmbeddingCoverage
		if err := rows.Scan(&coverage.Collection, &coverage.Fingerprint, &coverage.Chunks, &coverage.Covered); err != nil {
			return nil, fmt.Errorf("scan knowledge embedding coverage: %w", err)
		}
		out = append(out, coverage)
	}
	if err := rows.Err(); err != nil {
		return nil, classifyBusy(fmt.Errorf("list knowledge embedding coverage: %w", err))
	}
	return out, nil
}

func KBEmbeddingPresent(ctx context.Context, db *sql.DB) (bool, error) {
	if db == nil {
		return false, errNilArgument("db")
	}
	var present int
	err := db.QueryRowContext(ctx,
		`SELECT (SELECT count(*) FROM pragma_table_info('kb_collection') WHERE name = 'embedding_fingerprint')
		      + (SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'kb_embedding')`).Scan(&present)
	if err != nil {
		return false, classifyBusy(fmt.Errorf("probe knowledge embedding schema: %w", err))
	}
	return present == 2, nil
}
