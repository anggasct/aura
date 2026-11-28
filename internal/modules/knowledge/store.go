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
	"time"

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

func parseOptionalTime(value sql.NullString) (time.Time, bool, error) {
	if !value.Valid || value.String == "" {
		return time.Time{}, false, nil
	}
	parsed, err := sqlite.ParseTime(value.String)
	if err != nil {
		return time.Time{}, false, err
	}
	return parsed, true, nil
}

func (s *SQLiteStore) ListCollections(ctx context.Context) ([]CollectionSummary, error) {
	if s == nil || s.db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx,
		`SELECT c.id, c.generation, c.created_at, c.trust, c.trusted_at, count(DISTINCT s.id), count(k.id)
		 FROM kb_collection c
		 JOIN kb_source s ON s.collection = c.id
		 LEFT JOIN kb_chunk k ON k.source_id = s.id
		 GROUP BY c.id, c.generation, c.created_at, c.trust, c.trusted_at
		 ORDER BY c.id`)
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list knowledge collections: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var out []CollectionSummary
	for rows.Next() {
		var summary CollectionSummary
		var created string
		var trustedAt sql.NullString
		if err := rows.Scan(&summary.ID, &summary.Generation, &created, &summary.Trust, &trustedAt, &summary.Sources, &summary.Chunks); err != nil {
			return nil, sqlite.Classify(fmt.Errorf("scan knowledge collection: %w", err))
		}
		if summary.CreatedAt, err = sqlite.ParseTime(created); err != nil {
			return nil, fmt.Errorf("parse knowledge collection time: %w", err)
		}
		if parsed, ok, err := parseOptionalTime(trustedAt); err != nil {
			return nil, fmt.Errorf("parse knowledge collection trust time: %w", err)
		} else if ok {
			summary.TrustedAt = &parsed
		}
		out = append(out, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list knowledge collections: %w", err))
	}
	return out, nil
}

func (s *SQLiteStore) GetCollection(ctx context.Context, collection string) (CollectionSummary, bool, error) {
	if s == nil || s.db == nil {
		return CollectionSummary{}, false, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	var summary CollectionSummary
	var created string
	var trustedAt sql.NullString
	err := sqlite.Conn(ctx, s.db).QueryRowContext(ctx,
		`SELECT c.id, c.generation, c.created_at, c.trust, c.trusted_at,
		        (SELECT count(*) FROM kb_source s WHERE s.collection = c.id),
		        (SELECT count(*) FROM kb_chunk k JOIN kb_source s ON s.id = k.source_id WHERE s.collection = c.id)
		 FROM kb_collection c
		 WHERE c.id = ?`, collection).Scan(
		&summary.ID, &summary.Generation, &created, &summary.Trust, &trustedAt, &summary.Sources, &summary.Chunks,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return CollectionSummary{}, false, nil
	}
	if err != nil {
		return CollectionSummary{}, false, sqlite.Classify(fmt.Errorf("get knowledge collection: %w", err))
	}
	if summary.CreatedAt, err = sqlite.ParseTime(created); err != nil {
		return CollectionSummary{}, false, fmt.Errorf("parse knowledge collection time: %w", err)
	}
	if parsed, ok, err := parseOptionalTime(trustedAt); err != nil {
		return CollectionSummary{}, false, fmt.Errorf("parse knowledge collection trust time: %w", err)
	} else if ok {
		summary.TrustedAt = &parsed
	}
	return summary, true, nil
}

func (s *SQLiteStore) TrustCollection(ctx context.Context, collection string, at time.Time) (TrustOutcome, error) {
	if s == nil || s.db == nil {
		return TrustOutcome{}, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if collection == "" || at.IsZero() {
		return TrustOutcome{}, Errorf(ErrorCodeInvalidArgument, "collection and time are required")
	}
	var outcome TrustOutcome
	err := sqlite.RunInTx(ctx, s.db, func(txCtx context.Context) error {
		conn := sqlite.Conn(txCtx, s.db)
		var sources int
		if err := conn.QueryRowContext(txCtx, `SELECT count(*) FROM kb_source WHERE collection = ?`, collection).Scan(&sources); err != nil {
			return sqlite.Classify(fmt.Errorf("count knowledge sources: %w", err))
		}
		if sources == 0 {
			return Errorf(ErrorCodeSourceNotFound, "collection has no sources")
		}
		stamp := sqlite.FormatTime(at.UTC())
		res, err := conn.ExecContext(txCtx,
			`UPDATE kb_collection SET trust = ?, trusted_at = ?, generation = generation + 1 WHERE id = ?`,
			TrustTrusted, stamp, collection)
		if err != nil {
			return sqlite.Classify(fmt.Errorf("trust knowledge collection: %w", err))
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return sqlite.Classify(fmt.Errorf("trust knowledge collection rows: %w", err))
		}
		if affected == 0 {
			return Errorf(ErrorCodeSourceNotFound, "collection has no sources")
		}
		vouchedRes, err := conn.ExecContext(txCtx,
			`UPDATE kb_source SET vouched_sha256 = content_sha256, vouched_at = ? WHERE collection = ?`,
			stamp, collection)
		if err != nil {
			return sqlite.Classify(fmt.Errorf("vouch knowledge sources: %w", err))
		}
		vouched, err := vouchedRes.RowsAffected()
		if err != nil {
			return sqlite.Classify(fmt.Errorf("vouch knowledge sources rows: %w", err))
		}
		outcome = TrustOutcome{Collection: collection, TrustedAt: at.UTC(), Vouched: vouched}
		return nil
	})
	if err != nil {
		return TrustOutcome{}, err
	}
	return outcome, nil
}

func (s *SQLiteStore) UntrustCollection(ctx context.Context, collection string) (UntrustOutcome, error) {
	if s == nil || s.db == nil {
		return UntrustOutcome{}, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if collection == "" {
		return UntrustOutcome{}, Errorf(ErrorCodeInvalidArgument, "collection is required")
	}
	var outcome UntrustOutcome
	err := sqlite.RunInTx(ctx, s.db, func(txCtx context.Context) error {
		conn := sqlite.Conn(txCtx, s.db)
		res, err := conn.ExecContext(txCtx,
			`UPDATE kb_collection SET trust = ?, trusted_at = NULL, generation = generation + 1 WHERE id = ?`,
			TrustUntrusted, collection)
		if err != nil {
			return sqlite.Classify(fmt.Errorf("untrust knowledge collection: %w", err))
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return sqlite.Classify(fmt.Errorf("untrust knowledge collection rows: %w", err))
		}
		if affected == 0 {
			return Errorf(ErrorCodeSourceNotFound, "knowledge collection does not exist")
		}
		clearedRes, err := conn.ExecContext(txCtx,
			`UPDATE kb_source SET vouched_sha256 = NULL, vouched_at = NULL WHERE collection = ?`,
			collection)
		if err != nil {
			return sqlite.Classify(fmt.Errorf("clear knowledge source vouching: %w", err))
		}
		cleared, err := clearedRes.RowsAffected()
		if err != nil {
			return sqlite.Classify(fmt.Errorf("clear knowledge source vouching rows: %w", err))
		}
		outcome = UntrustOutcome{Collection: collection, Cleared: cleared}
		return nil
	})
	if err != nil {
		return UntrustOutcome{}, err
	}
	return outcome, nil
}

const selectSourceColumns = `s.id, s.collection, s.kind, s.uri, s.label, s.content_sha256, coalesce(s.vouched_sha256, ''), s.vouched_at, s.ingest_version, s.ingested_at,
	(SELECT count(*) FROM kb_chunk k WHERE k.source_id = s.id)`

func scanSource(row rowScanner) (SourceRecord, error) {
	var source SourceRecord
	var vouchedAt sql.NullString
	var ingested string
	if err := row.Scan(&source.ID, &source.Collection, &source.Kind, &source.URI, &source.Label,
		&source.ContentSHA256, &source.VouchedSHA256, &vouchedAt, &source.IngestVersion, &ingested, &source.Chunks); err != nil {
		return SourceRecord{}, err
	}
	if parsed, ok, err := parseOptionalTime(vouchedAt); err != nil {
		return SourceRecord{}, fmt.Errorf("parse knowledge source vouch time: %w", err)
	} else if ok {
		source.VouchedAt = &parsed
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

func (s *SQLiteStore) DeleteSource(ctx context.Context, collection, uri string) (RemoveOutcome, error) {
	if s == nil || s.db == nil {
		return RemoveOutcome{}, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if !sqlite.InTx(ctx, s.db) {
		return RemoveOutcome{}, Errorf(ErrorCodeInvalidArgument, "remove source runs inside a transaction")
	}
	conn := sqlite.Conn(ctx, s.db)
	restore, err := enableSecureDelete(ctx, conn)
	if err != nil {
		return RemoveOutcome{}, err
	}
	defer restore()
	return deleteSource(ctx, conn, collection, uri)
}

func (s *SQLiteStore) TruncateWAL(ctx context.Context) (bool, error) {
	if s == nil || s.db == nil {
		return false, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if sqlite.InTx(ctx, s.db) {
		return false, Errorf(ErrorCodeInvalidArgument, "truncate write-ahead log cannot run inside an active transaction")
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return false, sqlite.Classify(fmt.Errorf("open knowledge connection: %w", err))
	}
	defer func() { _ = conn.Close() }()
	return truncateWAL(ctx, conn)
}

func enableSecureDelete(ctx context.Context, conn sqlite.Querier) (restore func(), err error) {
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

func deleteSource(ctx context.Context, conn sqlite.Querier, collection, uri string) (RemoveOutcome, error) {
	source, err := scanSource(conn.QueryRowContext(ctx,
		`SELECT `+selectSourceColumns+` FROM kb_source s WHERE s.collection = ? AND s.uri = ?`, collection, uri))
	if errors.Is(err, sql.ErrNoRows) {
		return RemoveOutcome{}, Errorf(ErrorCodeSourceNotFound, "knowledge source does not exist")
	}
	if err != nil {
		return RemoveOutcome{}, sqlite.Classify(fmt.Errorf("get knowledge source: %w", err))
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM kb_source WHERE id = ?`, source.ID); err != nil {
		return RemoveOutcome{}, sqlite.Classify(fmt.Errorf("delete knowledge source: %w", err))
	}
	if _, err := conn.ExecContext(ctx, `UPDATE kb_collection SET generation = generation + 1 WHERE id = ?`, collection); err != nil {
		return RemoveOutcome{}, sqlite.Classify(fmt.Errorf("advance knowledge collection generation: %w", err))
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
