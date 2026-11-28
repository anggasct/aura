package knowledge

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const (
	kbVectorValueBytes = 4
	kbUnitTolerance    = 1e-3
)

type EmbeddingCoverage struct {
	Collection  string
	Fingerprint string
	Chunks      int
	Covered     int
}

const kbPendingJoin = `FROM kb_chunk c
	JOIN kb_source s ON s.id = c.source_id
	LEFT JOIN kb_embedding e ON e.chunk_rowid = c.rowid AND e.fingerprint = ? AND e.content_sha256 = c.content_sha256
	WHERE s.collection = ?`

func encodeVector(vector []float32) []byte {
	blob := make([]byte, len(vector)*kbVectorValueBytes)
	for i, value := range vector {
		binary.LittleEndian.PutUint32(blob[i*kbVectorValueBytes:], math.Float32bits(value))
	}
	return blob
}

func validateVector(vector []float32) error {
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

func (s *SQLiteStore) EmbeddingState(ctx context.Context, collection, fingerprint string) (EmbeddingState, error) {
	if s == nil || s.db == nil {
		return EmbeddingState{}, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	conn := sqlite.Conn(ctx, s.db)
	var stored sql.NullString
	err := conn.QueryRowContext(ctx, `SELECT embedding_fingerprint FROM kb_collection WHERE id = ?`, collection).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return EmbeddingState{}, nil
	}
	if err != nil {
		return EmbeddingState{}, sqlite.Classify(fmt.Errorf("read knowledge embedding state: %w", err))
	}
	state := EmbeddingState{Exists: true, Fingerprint: stored.String}
	if err := conn.QueryRowContext(ctx,
		`SELECT count(*), count(e.chunk_rowid), coalesce(sum(CASE WHEN e.chunk_rowid IS NULL THEN length(coalesce(c.heading, '')) + length(c.content) ELSE 0 END), 0) `+kbPendingJoin,
		fingerprint, collection).Scan(&state.Chunks, &state.Covered, &state.PendingCharacters); err != nil {
		return EmbeddingState{}, sqlite.Classify(fmt.Errorf("count knowledge embedding coverage: %w", err))
	}
	return state, nil
}

func (s *SQLiteStore) StartEmbedding(ctx context.Context, collection, fingerprint string) error {
	if s == nil || s.db == nil {
		return fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if fingerprint == "" {
		return Errorf(ErrorCodeInvalidArgument, "embedding fingerprint must not be empty")
	}
	result, err := sqlite.Conn(ctx, s.db).ExecContext(ctx, `UPDATE kb_collection SET embedding_fingerprint = ? WHERE id = ?`, fingerprint, collection)
	if err != nil {
		return sqlite.Classify(fmt.Errorf("record knowledge embedding opt-in: %w", err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return sqlite.Classify(fmt.Errorf("count knowledge embedding opt-in: %w", err))
	}
	if affected != 1 {
		return Errorf(ErrorCodeSourceNotFound, "knowledge collection does not exist")
	}
	return nil
}

func (s *SQLiteStore) PendingChunks(ctx context.Context, collection, fingerprint string, afterRowID int64, limit int) ([]PendingChunk, error) {
	if s == nil || s.db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if limit <= 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "pending chunk limit must be positive")
	}
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx,
		`SELECT c.rowid, coalesce(c.heading, ''), c.content, c.content_sha256 `+kbPendingJoin+` AND e.chunk_rowid IS NULL AND c.rowid > ? ORDER BY c.rowid LIMIT ?`,
		fingerprint, collection, afterRowID, limit)
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list pending knowledge chunks: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var pending []PendingChunk
	for rows.Next() {
		var chunk PendingChunk
		if err := rows.Scan(&chunk.RowID, &chunk.Heading, &chunk.Content, &chunk.SHA256); err != nil {
			return nil, sqlite.Classify(fmt.Errorf("scan pending knowledge chunk: %w", err))
		}
		pending = append(pending, chunk)
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list pending knowledge chunks: %w", err))
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

func (s *SQLiteStore) CommitVectors(ctx context.Context, collection, fingerprint string, vectors []VectorWrite) (CommitOutcome, error) {
	if s == nil || s.db == nil {
		return CommitOutcome{}, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if fingerprint == "" || len(vectors) == 0 {
		return CommitOutcome{}, Errorf(ErrorCodeInvalidArgument, "embedding fingerprint and vectors are required")
	}
	for i := range vectors {
		if len(vectors[i].ContentSHA256) != kbSHA256HexLen {
			return CommitOutcome{}, Errorf(ErrorCodeInvalidArgument, "knowledge vector needs the digest of the chunk it was computed from")
		}
		if err := validateVector(vectors[i].Vector); err != nil {
			return CommitOutcome{}, err
		}
	}
	var outcome CommitOutcome
	err := sqlite.RunInTx(ctx, s.db, func(txCtx context.Context) error {
		conn := sqlite.Conn(txCtx, s.db)
		for i := range vectors {
			result, err := conn.ExecContext(txCtx, kbCommitVectorSQL,
				fingerprint, encodeVector(vectors[i].Vector), vectors[i].RowID, vectors[i].ContentSHA256, collection, fingerprint)
			if err != nil {
				return sqlite.Classify(fmt.Errorf("write knowledge vector: %w", err))
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return sqlite.Classify(fmt.Errorf("count knowledge vector write: %w", err))
			}
			if affected != 1 {
				return Errorf(ErrorCodeEmbeddingFailed, "knowledge chunk or opt-in changed while its vector was computed")
			}
		}
		completed, err := completeEmbedding(txCtx, conn, collection, fingerprint)
		if err != nil {
			return err
		}
		outcome = CommitOutcome{Written: len(vectors), Completed: completed}
		return nil
	})
	if err != nil {
		return CommitOutcome{}, sqlite.Classify(err)
	}
	return outcome, nil
}

func completeEmbedding(ctx context.Context, conn sqlite.Querier, collection, fingerprint string) (bool, error) {
	var remaining bool
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 `+kbPendingJoin+` AND e.chunk_rowid IS NULL)`, fingerprint, collection).Scan(&remaining); err != nil {
		return false, sqlite.Classify(fmt.Errorf("check knowledge embedding coverage: %w", err))
	}
	if remaining {
		return false, nil
	}
	if _, err := conn.ExecContext(ctx, `UPDATE kb_collection SET generation = generation + 1 WHERE id = ? AND embedding_fingerprint = ?`, collection, fingerprint); err != nil {
		return false, sqlite.Classify(fmt.Errorf("advance knowledge collection generation: %w", err))
	}
	return true, nil
}

func (s *SQLiteStore) DisableEmbedding(ctx context.Context, collection string) (DisableOutcome, error) {
	if s == nil || s.db == nil {
		return DisableOutcome{}, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if !sqlite.InTx(ctx, s.db) {
		return DisableOutcome{}, Errorf(ErrorCodeInvalidArgument, "disable embedding runs inside a transaction")
	}
	conn := sqlite.Conn(ctx, s.db)
	restore, err := enableSecureDelete(ctx, conn)
	if err != nil {
		return DisableOutcome{}, err
	}
	defer restore()
	return deleteVectors(ctx, conn, collection)
}

const kbCollectionVectorsSQL = `FROM kb_embedding WHERE chunk_rowid IN (
	SELECT c.rowid FROM kb_chunk c JOIN kb_source s ON s.id = c.source_id WHERE s.collection = ?)`

func (s *SQLiteStore) PreviewDisableEmbedding(ctx context.Context, collection string) (DisableOutcome, error) {
	conn := sqlite.Conn(ctx, s.db)
	var stored sql.NullString
	err := conn.QueryRowContext(ctx, `SELECT embedding_fingerprint FROM kb_collection WHERE id = ?`, collection).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return DisableOutcome{}, Errorf(ErrorCodeSourceNotFound, "knowledge collection does not exist")
	}
	if err != nil {
		return DisableOutcome{}, sqlite.Classify(fmt.Errorf("read knowledge embedding state: %w", err))
	}
	result := DisableOutcome{OptedIn: stored.Valid}
	if err := conn.QueryRowContext(ctx, `SELECT count(*) `+kbCollectionVectorsSQL, collection).Scan(&result.Vectors); err != nil {
		return DisableOutcome{}, sqlite.Classify(fmt.Errorf("count knowledge vectors: %w", err))
	}
	return result, nil
}

func deleteVectors(ctx context.Context, tx sqlite.Querier, collection string) (DisableOutcome, error) {
	var stored sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT embedding_fingerprint FROM kb_collection WHERE id = ?`, collection).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return DisableOutcome{}, Errorf(ErrorCodeSourceNotFound, "knowledge collection does not exist")
	}
	if err != nil {
		return DisableOutcome{}, sqlite.Classify(fmt.Errorf("read knowledge embedding state: %w", err))
	}
	deleted, err := tx.ExecContext(ctx, `DELETE `+kbCollectionVectorsSQL, collection)
	if err != nil {
		return DisableOutcome{}, sqlite.Classify(fmt.Errorf("delete knowledge vectors: %w", err))
	}
	vectors, err := deleted.RowsAffected()
	if err != nil {
		return DisableOutcome{}, sqlite.Classify(fmt.Errorf("count deleted knowledge vectors: %w", err))
	}
	if stored.Valid || vectors > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE kb_collection SET embedding_fingerprint = NULL, generation = generation + 1 WHERE id = ?`, collection); err != nil {
			return DisableOutcome{}, sqlite.Classify(fmt.Errorf("clear knowledge embedding opt-in: %w", err))
		}
	}
	return DisableOutcome{OptedIn: stored.Valid, Vectors: int(vectors)}, nil
}

func (s *SQLiteStore) EmbeddingCoverage(ctx context.Context, fingerprint string) ([]EmbeddingCoverage, error) {
	if s == nil || s.db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx,
		`SELECT k.id, k.embedding_fingerprint, count(c.rowid), count(e.chunk_rowid)
		 FROM kb_collection k
		 LEFT JOIN kb_source s ON s.collection = k.id
		 LEFT JOIN kb_chunk c ON c.source_id = s.id
		 LEFT JOIN kb_embedding e ON e.chunk_rowid = c.rowid AND e.fingerprint = ? AND e.content_sha256 = c.content_sha256
		 WHERE k.embedding_fingerprint IS NOT NULL
		 GROUP BY k.id, k.embedding_fingerprint
		 ORDER BY k.id`, fingerprint)
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list knowledge embedding coverage: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var out []EmbeddingCoverage
	for rows.Next() {
		var coverage EmbeddingCoverage
		if err := rows.Scan(&coverage.Collection, &coverage.Fingerprint, &coverage.Chunks, &coverage.Covered); err != nil {
			return nil, sqlite.Classify(fmt.Errorf("scan knowledge embedding coverage: %w", err))
		}
		out = append(out, coverage)
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list knowledge embedding coverage: %w", err))
	}
	return out, nil
}

func EmbeddingPresent(ctx context.Context, db *sql.DB) (bool, error) {
	if db == nil {
		return false, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	var present int
	err := sqlite.Conn(ctx, db).QueryRowContext(ctx,
		`SELECT (SELECT count(*) FROM pragma_table_info('kb_collection') WHERE name = 'embedding_fingerprint')
		      + (SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'kb_embedding')`).Scan(&present)
	if err != nil {
		return false, sqlite.Classify(fmt.Errorf("probe knowledge embedding schema: %w", err))
	}
	return present == 2, nil
}
