package knowledge

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const (
	kbScopeHashBytes = 16
	kbSHA256HexLen   = 64
)

type rowScanner interface {
	Scan(dest ...any) error
}

func nullText(value string) sql.NullString {
	if value == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: value, Valid: true}
}

type SQLiteStore struct {
	db *sql.DB
}

var (
	_ Store          = (*SQLiteStore)(nil)
	_ EmbeddingStore = (*SQLiteStore)(nil)
)

func NewSQLiteStore(db *sql.DB) (*SQLiteStore, error) {
	if db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	return &SQLiteStore{db: db}, nil
}

func ScopeToken(collection string) string {
	sum := sha256.Sum256([]byte(collection))
	return "kb" + hex.EncodeToString(sum[:kbScopeHashBytes]) + "0"
}

func newKBID(prefix string) string {
	return prefix + strings.ToLower(rand.Text())
}

func validateSourceWrite(write *SourceWrite) error {
	switch {
	case write.Collection == "" || write.URI == "" || write.Label == "":
		return Errorf(ErrorCodeInvalidArgument, "knowledge source identity must not be empty")
	case write.Kind != SourceKindFile && write.Kind != SourceKindURL:
		return Errorf(ErrorCodeInvalidArgument, "knowledge source kind is not valid")
	case len(write.ContentSHA256) != kbSHA256HexLen || write.IngestVersion <= 0 || write.IngestedAt.IsZero():
		return Errorf(ErrorCodeInvalidArgument, "knowledge source digest, version and time are required")
	case len(write.Chunks) == 0:
		return Errorf(ErrorCodeInvalidArgument, "knowledge source must carry at least one chunk")
	case write.CollectionCap <= 0 || write.TotalCap <= 0:
		return Errorf(ErrorCodeInvalidArgument, "knowledge chunk limits must be positive")
	}
	for i := range write.Chunks {
		chunk := &write.Chunks[i]
		if chunk.Ordinal != i || chunk.Content == "" || len(chunk.SHA256) != kbSHA256HexLen {
			return Errorf(ErrorCodeInvalidArgument, "knowledge chunks must be dense, non-empty and hashed")
		}
	}
	return nil
}

type kbChunkKey struct {
	ordinal int
	sha256  string
}

func (s *SQLiteStore) ReplaceSource(ctx context.Context, write *SourceWrite) (WriteResult, error) {
	if s == nil || s.db == nil {
		return WriteResult{}, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if write == nil {
		return WriteResult{}, fault.New(ErrorCodeInvalidArgument, "write must not be nil")
	}
	if err := validateSourceWrite(write); err != nil {
		return WriteResult{}, err
	}
	var result WriteResult
	err := sqlite.RunInTx(ctx, s.db, func(txCtx context.Context) error {
		var txErr error
		result, txErr = replaceSource(txCtx, sqlite.Conn(txCtx, s.db), write)
		return txErr
	})
	if err != nil {
		return WriteResult{}, sqlite.Classify(err)
	}
	return result, nil
}

func replaceSource(ctx context.Context, conn sqlite.Querier, write *SourceWrite) (WriteResult, error) {
	sourceID, existingSHA, existingVersion, found, err := lookupSourceForWrite(ctx, conn, write)
	if err != nil {
		return WriteResult{}, err
	}
	if found && existingSHA == write.ContentSHA256 && existingVersion == write.IngestVersion {
		return WriteResult{Status: StatusUnchanged, SourceID: sourceID, Chunks: len(write.Chunks), ChunksKept: len(write.Chunks)}, nil
	}
	stamp := sqlite.FormatTime(write.IngestedAt)
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO kb_collection (id, generation, created_at) VALUES (?, 0, ?) ON CONFLICT(id) DO NOTHING`,
		write.Collection, stamp); err != nil {
		return WriteResult{}, sqlite.Classify(fmt.Errorf("ensure knowledge collection: %w", err))
	}
	if err := checkCaps(ctx, conn, write, sourceID); err != nil {
		return WriteResult{}, err
	}
	status := StatusReplaced
	if !found {
		status = StatusAdded
		sourceID = newKBID("kbs_")
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO kb_source (id, collection, kind, uri, label, content_sha256, ingest_version, ingested_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			sourceID, write.Collection, write.Kind, write.URI, write.Label, write.ContentSHA256, write.IngestVersion, stamp); err != nil {
			return WriteResult{}, sqlite.Classify(fmt.Errorf("insert knowledge source: %w", err))
		}
	} else if _, err := conn.ExecContext(ctx,
		`UPDATE kb_source SET label = ?, content_sha256 = ?, ingest_version = ?, ingested_at = ? WHERE id = ?`,
		write.Label, write.ContentSHA256, write.IngestVersion, stamp, sourceID); err != nil {
		return WriteResult{}, sqlite.Classify(fmt.Errorf("update knowledge source: %w", err))
	}
	result, err := diffChunks(ctx, conn, write, sourceID)
	if err != nil {
		return WriteResult{}, err
	}
	result.Status = status
	result.SourceID = sourceID
	result.Chunks = len(write.Chunks)
	if _, err := conn.ExecContext(ctx, `UPDATE kb_collection SET generation = generation + 1 WHERE id = ?`, write.Collection); err != nil {
		return WriteResult{}, sqlite.Classify(fmt.Errorf("advance knowledge collection generation: %w", err))
	}
	return result, nil
}

func lookupSourceForWrite(ctx context.Context, conn sqlite.Querier, write *SourceWrite) (id, sha string, version int, found bool, err error) {
	err = conn.QueryRowContext(ctx,
		`SELECT id, content_sha256, ingest_version FROM kb_source WHERE collection = ? AND uri = ?`,
		write.Collection, write.URI).Scan(&id, &sha, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", 0, false, nil
	}
	if err != nil {
		return "", "", 0, false, sqlite.Classify(fmt.Errorf("look up knowledge source: %w", err))
	}
	return id, sha, version, true, nil
}

func checkCaps(ctx context.Context, conn sqlite.Querier, write *SourceWrite, sourceID string) error {
	var inCollection, inTotal int
	if err := conn.QueryRowContext(ctx,
		`SELECT count(*) FROM kb_chunk c JOIN kb_source s ON s.id = c.source_id WHERE s.collection = ? AND c.source_id <> ?`,
		write.Collection, sourceID).Scan(&inCollection); err != nil {
		return sqlite.Classify(fmt.Errorf("count collection chunks: %w", err))
	}
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM kb_chunk WHERE source_id <> ?`, sourceID).Scan(&inTotal); err != nil {
		return sqlite.Classify(fmt.Errorf("count total chunks: %w", err))
	}
	incoming := len(write.Chunks)
	if inCollection < 0 || inCollection > write.CollectionCap-incoming {
		return Errorf(ErrorCodeCapExceeded, "collection chunk cap would be exceeded")
	}
	if inTotal < 0 || inTotal > write.TotalCap-incoming {
		return Errorf(ErrorCodeCapExceeded, "total chunk cap would be exceeded")
	}
	return nil
}

func diffChunks(ctx context.Context, conn sqlite.Querier, write *SourceWrite, sourceID string) (WriteResult, error) {
	existing, err := loadChunkKeys(ctx, conn, sourceID)
	if err != nil {
		return WriteResult{}, err
	}
	wanted := make(map[kbChunkKey]struct{}, len(write.Chunks))
	for i := range write.Chunks {
		wanted[kbChunkKey{ordinal: write.Chunks[i].Ordinal, sha256: write.Chunks[i].SHA256}] = struct{}{}
	}
	var result WriteResult
	remove, err := conn.PrepareContext(ctx, `DELETE FROM kb_chunk WHERE id = ?`)
	if err != nil {
		return WriteResult{}, sqlite.Classify(fmt.Errorf("prepare knowledge chunk delete: %w", err))
	}
	defer func() { _ = remove.Close() }()
	for key, id := range existing {
		if _, keep := wanted[key]; keep {
			result.ChunksKept++
			continue
		}
		if _, err := remove.ExecContext(ctx, id); err != nil {
			return WriteResult{}, sqlite.Classify(fmt.Errorf("delete knowledge chunk: %w", err))
		}
		result.ChunksRemoved++
	}
	insert, err := conn.PrepareContext(ctx,
		`INSERT INTO kb_chunk (id, source_id, ordinal, heading, content, scope, content_sha256) VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return WriteResult{}, sqlite.Classify(fmt.Errorf("prepare knowledge chunk insert: %w", err))
	}
	defer func() { _ = insert.Close() }()
	scope := ScopeToken(write.Collection)
	for i := range write.Chunks {
		chunk := &write.Chunks[i]
		if _, kept := existing[kbChunkKey{ordinal: chunk.Ordinal, sha256: chunk.SHA256}]; kept {
			continue
		}
		if _, err := insert.ExecContext(ctx, newKBID("kbc_"), sourceID, chunk.Ordinal, nullText(chunk.Heading), chunk.Content, scope, chunk.SHA256); err != nil {
			return WriteResult{}, sqlite.Classify(fmt.Errorf("insert knowledge chunk: %w", err))
		}
		result.ChunksAdded++
	}
	return result, nil
}

func loadChunkKeys(ctx context.Context, conn sqlite.Querier, sourceID string) (map[kbChunkKey]string, error) {
	rows, err := conn.QueryContext(ctx, `SELECT id, ordinal, content_sha256 FROM kb_chunk WHERE source_id = ?`, sourceID)
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list knowledge chunks: %w", err))
	}
	defer func() { _ = rows.Close() }()
	keys := make(map[kbChunkKey]string)
	for rows.Next() {
		var id string
		var key kbChunkKey
		if err := rows.Scan(&id, &key.ordinal, &key.sha256); err != nil {
			return nil, sqlite.Classify(fmt.Errorf("scan knowledge chunk: %w", err))
		}
		keys[key] = id
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list knowledge chunks: %w", err))
	}
	return keys, nil
}

func (s *SQLiteStore) ListCollections(ctx context.Context) ([]CollectionSummary, error) {
	if s == nil || s.db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx,
		`SELECT c.id, c.generation, c.created_at, count(DISTINCT s.id), count(k.id)
		 FROM kb_collection c
		 JOIN kb_source s ON s.collection = c.id
		 LEFT JOIN kb_chunk k ON k.source_id = s.id
		 GROUP BY c.id, c.generation, c.created_at
		 ORDER BY c.id`)
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list knowledge collections: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var out []CollectionSummary
	for rows.Next() {
		var summary CollectionSummary
		var created string
		if err := rows.Scan(&summary.ID, &summary.Generation, &created, &summary.Sources, &summary.Chunks); err != nil {
			return nil, sqlite.Classify(fmt.Errorf("scan knowledge collection: %w", err))
		}
		if summary.CreatedAt, err = sqlite.ParseTime(created); err != nil {
			return nil, fmt.Errorf("parse knowledge collection time: %w", err)
		}
		out = append(out, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list knowledge collections: %w", err))
	}
	return out, nil
}

const selectSourceColumns = `s.id, s.collection, s.kind, s.uri, s.label, s.content_sha256, s.ingest_version, s.ingested_at,
	(SELECT count(*) FROM kb_chunk k WHERE k.source_id = s.id)`

func scanSource(row rowScanner) (SourceRecord, error) {
	var source SourceRecord
	var ingested string
	if err := row.Scan(&source.ID, &source.Collection, &source.Kind, &source.URI, &source.Label,
		&source.ContentSHA256, &source.IngestVersion, &ingested, &source.Chunks); err != nil {
		return SourceRecord{}, err
	}
	at, err := sqlite.ParseTime(ingested)
	if err != nil {
		return SourceRecord{}, fmt.Errorf("parse knowledge source time: %w", err)
	}
	source.IngestedAt = at
	return source, nil
}

func (s *SQLiteStore) ListSources(ctx context.Context, collection string) ([]SourceRecord, error) {
	if s == nil || s.db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx,
		`SELECT `+selectSourceColumns+` FROM kb_source s WHERE s.collection = ? ORDER BY s.uri`, collection)
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list knowledge sources: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var out []SourceRecord
	for rows.Next() {
		source, err := scanSource(rows)
		if err != nil {
			return nil, fmt.Errorf("scan knowledge source: %w", err)
		}
		out = append(out, source)
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list knowledge sources: %w", err))
	}
	return out, nil
}

func (s *SQLiteStore) GetSource(ctx context.Context, collection, uri string) (SourceRecord, bool, error) {
	if s == nil || s.db == nil {
		return SourceRecord{}, false, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	source, err := scanSource(sqlite.Conn(ctx, s.db).QueryRowContext(ctx,
		`SELECT `+selectSourceColumns+` FROM kb_source s WHERE s.collection = ? AND s.uri = ?`, collection, uri))
	if errors.Is(err, sql.ErrNoRows) {
		return SourceRecord{}, false, nil
	}
	if err != nil {
		return SourceRecord{}, false, sqlite.Classify(fmt.Errorf("get knowledge source: %w", err))
	}
	return source, true, nil
}

func (s *SQLiteStore) RemoveSource(ctx context.Context, collection, uri string, apply bool) (RemoveOutcome, error) {
	if s == nil || s.db == nil {
		return RemoveOutcome{}, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if !apply {
		source, found, err := s.GetSource(ctx, collection, uri)
		if err != nil {
			return RemoveOutcome{}, err
		}
		if !found {
			return RemoveOutcome{}, Errorf(ErrorCodeSourceNotFound, "knowledge source does not exist")
		}
		return RemoveOutcome{SourceID: source.ID, Chunks: source.Chunks}, nil
	}
	if sqlite.InTx(ctx, s.db) {
		return RemoveOutcome{}, Errorf(ErrorCodeInvalidArgument, "remove source cannot run inside an active transaction")
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return RemoveOutcome{}, sqlite.Classify(fmt.Errorf("open knowledge connection: %w", err))
	}
	defer func() { _ = conn.Close() }()
	restore, err := enableSecureDelete(ctx, conn)
	if err != nil {
		return RemoveOutcome{}, err
	}
	defer restore()
	result, err := deleteSource(ctx, conn, collection, uri)
	if err != nil {
		return RemoveOutcome{}, err
	}
	result.Applied = true
	truncated, err := truncateWAL(ctx, conn)
	if err != nil {
		return result, err
	}
	result.WALTruncated = truncated
	return result, nil
}

func enableSecureDelete(ctx context.Context, conn *sql.Conn) (restore func(), err error) {
	var previous int
	if err := conn.QueryRowContext(ctx, `PRAGMA secure_delete`).Scan(&previous); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("read secure_delete: %w", err))
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA secure_delete = ON`); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("enable secure_delete: %w", err))
	}
	return func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), fmt.Sprintf("PRAGMA secure_delete = %d", previous))
	}, nil
}

func deleteSource(ctx context.Context, conn *sql.Conn, collection, uri string) (RemoveOutcome, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return RemoveOutcome{}, sqlite.Classify(err)
	}
	defer func() { _ = tx.Rollback() }()
	source, err := scanSource(tx.QueryRowContext(ctx,
		`SELECT `+selectSourceColumns+` FROM kb_source s WHERE s.collection = ? AND s.uri = ?`, collection, uri))
	if errors.Is(err, sql.ErrNoRows) {
		return RemoveOutcome{}, Errorf(ErrorCodeSourceNotFound, "knowledge source does not exist")
	}
	if err != nil {
		return RemoveOutcome{}, sqlite.Classify(fmt.Errorf("get knowledge source: %w", err))
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM kb_source WHERE id = ?`, source.ID); err != nil {
		return RemoveOutcome{}, sqlite.Classify(fmt.Errorf("delete knowledge source: %w", err))
	}
	if _, err := tx.ExecContext(ctx, `UPDATE kb_collection SET generation = generation + 1 WHERE id = ?`, collection); err != nil {
		return RemoveOutcome{}, sqlite.Classify(fmt.Errorf("advance knowledge collection generation: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return RemoveOutcome{}, sqlite.Classify(err)
	}
	return RemoveOutcome{SourceID: source.ID, Chunks: source.Chunks}, nil
}

func truncateWAL(ctx context.Context, conn *sql.Conn) (bool, error) {
	var busy, logFrames, checkpointed int
	if err := conn.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		return false, sqlite.Classify(fmt.Errorf("truncate write-ahead log: %w", err))
	}
	return busy == 0, nil
}

func (s *SQLiteStore) Check(ctx context.Context) (IndexReport, error) {
	if s == nil || s.db == nil {
		return IndexReport{}, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	conn := sqlite.Conn(ctx, s.db)
	var report IndexReport
	for _, count := range []struct {
		target *int
		query  string
	}{
		{&report.Collections, `SELECT count(*) FROM kb_collection`},
		{&report.Sources, `SELECT count(*) FROM kb_source`},
		{&report.Chunks, `SELECT count(*) FROM kb_chunk`},
		{&report.IndexedChunks, `SELECT count(*) FROM kb_chunk_fts_docsize`},
	} {
		if err := conn.QueryRowContext(ctx, count.query).Scan(count.target); err != nil {
			return IndexReport{}, sqlite.Classify(fmt.Errorf("count knowledge rows: %w", err))
		}
	}
	if report.Chunks != report.IndexedChunks {
		report.Problems = append(report.Problems, "chunk count differs from indexed row count")
	}
	scopeProblem, err := s.scopeMismatch(ctx)
	if err != nil {
		return IndexReport{}, err
	}
	if scopeProblem {
		report.Problems = append(report.Problems, "chunk scope does not match its collection")
	}
	damaged, err := s.indexDamaged(ctx)
	if err != nil {
		return IndexReport{}, err
	}
	if damaged {
		report.Problems = append(report.Problems, "full-text index does not match chunk content")
	}
	return report, nil
}

func (s *SQLiteStore) scopeMismatch(ctx context.Context) (bool, error) {
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx, `SELECT DISTINCT s.collection, c.scope FROM kb_chunk c JOIN kb_source s ON s.id = c.source_id`)
	if err != nil {
		return false, sqlite.Classify(fmt.Errorf("list knowledge scopes: %w", err))
	}
	defer func() { _ = rows.Close() }()
	mismatch := false
	for rows.Next() {
		var collection, scope string
		if err := rows.Scan(&collection, &scope); err != nil {
			return false, sqlite.Classify(fmt.Errorf("scan knowledge scope: %w", err))
		}
		if scope != ScopeToken(collection) {
			mismatch = true
		}
	}
	if err := rows.Err(); err != nil {
		return false, sqlite.Classify(fmt.Errorf("list knowledge scopes: %w", err))
	}
	return mismatch, nil
}

func (s *SQLiteStore) indexDamaged(ctx context.Context) (bool, error) {
	_, err := sqlite.Conn(ctx, s.db).ExecContext(ctx, `INSERT INTO kb_chunk_fts(kb_chunk_fts, rank) VALUES ('integrity-check', 1)`)
	if err == nil {
		return false, nil
	}
	if fault.HasCode(sqlite.Classify(err), sqlite.ErrorCodeCorrupt) {
		return true, nil
	}
	return false, sqlite.Classify(fmt.Errorf("check knowledge index: %w", err))
}
