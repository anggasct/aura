package store

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

	"modernc.org/sqlite"
)

const (
	KBSourceKindFile = "file"
	KBSourceKindURL  = "url"

	KBWriteAdded     = "added"
	KBWriteReplaced  = "replaced"
	KBWriteUnchanged = "unchanged"

	kbScopeHashBytes = 16
	kbSQLiteCorrupt  = 11
	kbSQLitePrimary  = 0xff
	kbSHA256HexLen   = 64
)

type KBChunk struct {
	Ordinal int
	Heading string
	Content string
	SHA256  string
}

type KBLimits struct {
	CollectionChunks int
	TotalChunks      int
}

type KBSourceWrite struct {
	Collection    string
	Kind          string
	URI           string
	Label         string
	ContentSHA256 string
	IngestVersion int
	IngestedAt    time.Time
	Chunks        []KBChunk
	Limits        KBLimits
}

type KBWriteResult struct {
	Status        string
	SourceID      string
	Chunks        int
	ChunksAdded   int
	ChunksRemoved int
	ChunksKept    int
}

type KBCollectionSummary struct {
	ID         string
	Generation int64
	Sources    int
	Chunks     int
	CreatedAt  time.Time
}

type KBSource struct {
	ID            string
	Collection    string
	Kind          string
	URI           string
	Label         string
	ContentSHA256 string
	IngestVersion int
	IngestedAt    time.Time
	Chunks        int
}

type KBRemoveResult struct {
	SourceID     string
	Chunks       int
	Applied      bool
	WALTruncated bool
}

type KBCheckReport struct {
	Collections   int
	Sources       int
	Chunks        int
	IndexedChunks int
	Problems      []string
}

type KBStore interface {
	ReplaceSource(ctx context.Context, write *KBSourceWrite) (KBWriteResult, error)
	ListCollections(ctx context.Context) ([]KBCollectionSummary, error)
	ListSources(ctx context.Context, collection string) ([]KBSource, error)
	GetSource(ctx context.Context, collection, uri string) (KBSource, bool, error)
	RemoveSource(ctx context.Context, collection, uri string, apply bool) (KBRemoveResult, error)
	Check(ctx context.Context) (KBCheckReport, error)
}

type sqliteKBStore struct {
	db *sql.DB
}

func NewKBStore(db *sql.DB) KBStore {
	return &sqliteKBStore{db: db}
}

func KBScope(collection string) string {
	sum := sha256.Sum256([]byte(collection))
	return "kb" + hex.EncodeToString(sum[:kbScopeHashBytes]) + "0"
}

func newKBID(prefix string) string {
	return prefix + strings.ToLower(rand.Text())
}

func validateKBWrite(write *KBSourceWrite) error {
	switch {
	case write.Collection == "" || write.URI == "" || write.Label == "":
		return Errorf(ErrorCodeInvalidArgument, "knowledge source identity must not be empty")
	case write.Kind != KBSourceKindFile && write.Kind != KBSourceKindURL:
		return Errorf(ErrorCodeInvalidArgument, "knowledge source kind is not valid")
	case len(write.ContentSHA256) != kbSHA256HexLen || write.IngestVersion <= 0 || write.IngestedAt.IsZero():
		return Errorf(ErrorCodeInvalidArgument, "knowledge source digest, version and time are required")
	case len(write.Chunks) == 0:
		return Errorf(ErrorCodeInvalidArgument, "knowledge source must carry at least one chunk")
	case write.Limits.CollectionChunks <= 0 || write.Limits.TotalChunks <= 0:
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

func (s *sqliteKBStore) ReplaceSource(ctx context.Context, write *KBSourceWrite) (KBWriteResult, error) {
	if s.db == nil {
		return KBWriteResult{}, errNilArgument("db")
	}
	if write == nil {
		return KBWriteResult{}, errNilArgument("write")
	}
	if err := validateKBWrite(write); err != nil {
		return KBWriteResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return KBWriteResult{}, classifyBusy(err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := replaceKBSource(ctx, tx, write)
	if err != nil {
		return KBWriteResult{}, classifyBusy(err)
	}
	if result.Status == KBWriteUnchanged {
		return result, nil
	}
	if err := tx.Commit(); err != nil {
		return KBWriteResult{}, classifyBusy(err)
	}
	return result, nil
}

func replaceKBSource(ctx context.Context, tx *sql.Tx, write *KBSourceWrite) (KBWriteResult, error) {
	stamp := formatTime(write.IngestedAt)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO kb_collection (id, generation, created_at) VALUES (?, 0, ?) ON CONFLICT(id) DO NOTHING`,
		write.Collection, stamp); err != nil {
		return KBWriteResult{}, fmt.Errorf("ensure knowledge collection: %w", err)
	}
	sourceID, existingSHA, existingVersion, found, err := lookupKBSourceForWrite(ctx, tx, write)
	if err != nil {
		return KBWriteResult{}, err
	}
	if found && existingSHA == write.ContentSHA256 && existingVersion == write.IngestVersion {
		return KBWriteResult{Status: KBWriteUnchanged, SourceID: sourceID, Chunks: len(write.Chunks), ChunksKept: len(write.Chunks)}, nil
	}
	if err := checkKBCaps(ctx, tx, write, sourceID); err != nil {
		return KBWriteResult{}, err
	}
	status := KBWriteReplaced
	if !found {
		status = KBWriteAdded
		sourceID = newKBID("kbs_")
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO kb_source (id, collection, kind, uri, label, content_sha256, ingest_version, ingested_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			sourceID, write.Collection, write.Kind, write.URI, write.Label, write.ContentSHA256, write.IngestVersion, stamp); err != nil {
			return KBWriteResult{}, fmt.Errorf("insert knowledge source: %w", err)
		}
	} else if _, err := tx.ExecContext(ctx,
		`UPDATE kb_source SET label = ?, content_sha256 = ?, ingest_version = ?, ingested_at = ? WHERE id = ?`,
		write.Label, write.ContentSHA256, write.IngestVersion, stamp, sourceID); err != nil {
		return KBWriteResult{}, fmt.Errorf("update knowledge source: %w", err)
	}
	result, err := diffKBChunks(ctx, tx, write, sourceID)
	if err != nil {
		return KBWriteResult{}, err
	}
	result.Status = status
	result.SourceID = sourceID
	result.Chunks = len(write.Chunks)
	if _, err := tx.ExecContext(ctx, `UPDATE kb_collection SET generation = generation + 1 WHERE id = ?`, write.Collection); err != nil {
		return KBWriteResult{}, fmt.Errorf("advance knowledge collection generation: %w", err)
	}
	return result, nil
}

func lookupKBSourceForWrite(ctx context.Context, tx *sql.Tx, write *KBSourceWrite) (id, sha string, version int, found bool, err error) {
	err = tx.QueryRowContext(ctx,
		`SELECT id, content_sha256, ingest_version FROM kb_source WHERE collection = ? AND uri = ?`,
		write.Collection, write.URI).Scan(&id, &sha, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", 0, false, nil
	}
	if err != nil {
		return "", "", 0, false, fmt.Errorf("look up knowledge source: %w", err)
	}
	return id, sha, version, true, nil
}

func checkKBCaps(ctx context.Context, tx *sql.Tx, write *KBSourceWrite, sourceID string) error {
	var inCollection, inTotal int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM kb_chunk c JOIN kb_source s ON s.id = c.source_id WHERE s.collection = ? AND c.source_id <> ?`,
		write.Collection, sourceID).Scan(&inCollection); err != nil {
		return fmt.Errorf("count collection chunks: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM kb_chunk WHERE source_id <> ?`, sourceID).Scan(&inTotal); err != nil {
		return fmt.Errorf("count total chunks: %w", err)
	}
	incoming := len(write.Chunks)
	if inCollection < 0 || inCollection > write.Limits.CollectionChunks-incoming {
		return Errorf(ErrorCodeKBCapExceeded, "collection chunk cap would be exceeded")
	}
	if inTotal < 0 || inTotal > write.Limits.TotalChunks-incoming {
		return Errorf(ErrorCodeKBCapExceeded, "total chunk cap would be exceeded")
	}
	return nil
}

func diffKBChunks(ctx context.Context, tx *sql.Tx, write *KBSourceWrite, sourceID string) (KBWriteResult, error) {
	existing, err := loadKBChunkKeys(ctx, tx, sourceID)
	if err != nil {
		return KBWriteResult{}, err
	}
	wanted := make(map[kbChunkKey]struct{}, len(write.Chunks))
	for i := range write.Chunks {
		wanted[kbChunkKey{ordinal: write.Chunks[i].Ordinal, sha256: write.Chunks[i].SHA256}] = struct{}{}
	}
	var result KBWriteResult
	remove, err := tx.PrepareContext(ctx, `DELETE FROM kb_chunk WHERE id = ?`)
	if err != nil {
		return KBWriteResult{}, fmt.Errorf("prepare knowledge chunk delete: %w", err)
	}
	defer func() { _ = remove.Close() }()
	for key, id := range existing {
		if _, keep := wanted[key]; keep {
			result.ChunksKept++
			continue
		}
		if _, err := remove.ExecContext(ctx, id); err != nil {
			return KBWriteResult{}, fmt.Errorf("delete knowledge chunk: %w", err)
		}
		result.ChunksRemoved++
	}
	insert, err := tx.PrepareContext(ctx,
		`INSERT INTO kb_chunk (id, source_id, ordinal, heading, content, scope, content_sha256) VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return KBWriteResult{}, fmt.Errorf("prepare knowledge chunk insert: %w", err)
	}
	defer func() { _ = insert.Close() }()
	scope := KBScope(write.Collection)
	for i := range write.Chunks {
		chunk := &write.Chunks[i]
		if _, kept := existing[kbChunkKey{ordinal: chunk.Ordinal, sha256: chunk.SHA256}]; kept {
			continue
		}
		if _, err := insert.ExecContext(ctx, newKBID("kbc_"), sourceID, chunk.Ordinal, nullMemoryText(chunk.Heading), chunk.Content, scope, chunk.SHA256); err != nil {
			return KBWriteResult{}, fmt.Errorf("insert knowledge chunk: %w", err)
		}
		result.ChunksAdded++
	}
	return result, nil
}

func loadKBChunkKeys(ctx context.Context, tx *sql.Tx, sourceID string) (map[kbChunkKey]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, ordinal, content_sha256 FROM kb_chunk WHERE source_id = ?`, sourceID)
	if err != nil {
		return nil, fmt.Errorf("list knowledge chunks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	keys := make(map[kbChunkKey]string)
	for rows.Next() {
		var id string
		var key kbChunkKey
		if err := rows.Scan(&id, &key.ordinal, &key.sha256); err != nil {
			return nil, fmt.Errorf("scan knowledge chunk: %w", err)
		}
		keys[key] = id
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list knowledge chunks: %w", err)
	}
	return keys, nil
}

func (s *sqliteKBStore) ListCollections(ctx context.Context) ([]KBCollectionSummary, error) {
	if s.db == nil {
		return nil, errNilArgument("db")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.id, c.generation, c.created_at, count(DISTINCT s.id), count(k.id)
		 FROM kb_collection c
		 JOIN kb_source s ON s.collection = c.id
		 LEFT JOIN kb_chunk k ON k.source_id = s.id
		 GROUP BY c.id, c.generation, c.created_at
		 ORDER BY c.id`)
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("list knowledge collections: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var out []KBCollectionSummary
	for rows.Next() {
		var summary KBCollectionSummary
		var created string
		if err := rows.Scan(&summary.ID, &summary.Generation, &created, &summary.Sources, &summary.Chunks); err != nil {
			return nil, fmt.Errorf("scan knowledge collection: %w", err)
		}
		if summary.CreatedAt, err = parseTime(created); err != nil {
			return nil, fmt.Errorf("parse knowledge collection time: %w", err)
		}
		out = append(out, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, classifyBusy(fmt.Errorf("list knowledge collections: %w", err))
	}
	return out, nil
}

const selectKBSourceColumns = `s.id, s.collection, s.kind, s.uri, s.label, s.content_sha256, s.ingest_version, s.ingested_at,
	(SELECT count(*) FROM kb_chunk k WHERE k.source_id = s.id)`

func scanKBSource(row rowScanner) (KBSource, error) {
	var source KBSource
	var ingested string
	if err := row.Scan(&source.ID, &source.Collection, &source.Kind, &source.URI, &source.Label,
		&source.ContentSHA256, &source.IngestVersion, &ingested, &source.Chunks); err != nil {
		return KBSource{}, err
	}
	at, err := parseTime(ingested)
	if err != nil {
		return KBSource{}, fmt.Errorf("parse knowledge source time: %w", err)
	}
	source.IngestedAt = at
	return source, nil
}

func (s *sqliteKBStore) ListSources(ctx context.Context, collection string) ([]KBSource, error) {
	if s.db == nil {
		return nil, errNilArgument("db")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+selectKBSourceColumns+` FROM kb_source s WHERE s.collection = ? ORDER BY s.uri`, collection)
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("list knowledge sources: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var out []KBSource
	for rows.Next() {
		source, err := scanKBSource(rows)
		if err != nil {
			return nil, fmt.Errorf("scan knowledge source: %w", err)
		}
		out = append(out, source)
	}
	if err := rows.Err(); err != nil {
		return nil, classifyBusy(fmt.Errorf("list knowledge sources: %w", err))
	}
	return out, nil
}

func (s *sqliteKBStore) GetSource(ctx context.Context, collection, uri string) (KBSource, bool, error) {
	if s.db == nil {
		return KBSource{}, false, errNilArgument("db")
	}
	source, err := scanKBSource(s.db.QueryRowContext(ctx,
		`SELECT `+selectKBSourceColumns+` FROM kb_source s WHERE s.collection = ? AND s.uri = ?`, collection, uri))
	if errors.Is(err, sql.ErrNoRows) {
		return KBSource{}, false, nil
	}
	if err != nil {
		return KBSource{}, false, classifyBusy(fmt.Errorf("get knowledge source: %w", err))
	}
	return source, true, nil
}

func (s *sqliteKBStore) RemoveSource(ctx context.Context, collection, uri string, apply bool) (KBRemoveResult, error) {
	if s.db == nil {
		return KBRemoveResult{}, errNilArgument("db")
	}
	if !apply {
		source, found, err := s.GetSource(ctx, collection, uri)
		if err != nil {
			return KBRemoveResult{}, err
		}
		if !found {
			return KBRemoveResult{}, Errorf(ErrorCodeKBSourceNotFound, "knowledge source does not exist")
		}
		return KBRemoveResult{SourceID: source.ID, Chunks: source.Chunks}, nil
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return KBRemoveResult{}, classifyBusy(fmt.Errorf("open knowledge connection: %w", err))
	}
	defer func() { _ = conn.Close() }()
	restore, err := enableSecureDelete(ctx, conn)
	if err != nil {
		return KBRemoveResult{}, err
	}
	defer restore()
	result, err := deleteKBSource(ctx, conn, collection, uri)
	if err != nil {
		return KBRemoveResult{}, err
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
		return nil, classifyBusy(fmt.Errorf("read secure_delete: %w", err))
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA secure_delete = ON`); err != nil {
		return nil, classifyBusy(fmt.Errorf("enable secure_delete: %w", err))
	}
	return func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), fmt.Sprintf("PRAGMA secure_delete = %d", previous))
	}, nil
}

func deleteKBSource(ctx context.Context, conn *sql.Conn, collection, uri string) (KBRemoveResult, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return KBRemoveResult{}, classifyBusy(err)
	}
	defer func() { _ = tx.Rollback() }()
	source, err := scanKBSource(tx.QueryRowContext(ctx,
		`SELECT `+selectKBSourceColumns+` FROM kb_source s WHERE s.collection = ? AND s.uri = ?`, collection, uri))
	if errors.Is(err, sql.ErrNoRows) {
		return KBRemoveResult{}, Errorf(ErrorCodeKBSourceNotFound, "knowledge source does not exist")
	}
	if err != nil {
		return KBRemoveResult{}, classifyBusy(fmt.Errorf("get knowledge source: %w", err))
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM kb_source WHERE id = ?`, source.ID); err != nil {
		return KBRemoveResult{}, classifyBusy(fmt.Errorf("delete knowledge source: %w", err))
	}
	if _, err := tx.ExecContext(ctx, `UPDATE kb_collection SET generation = generation + 1 WHERE id = ?`, collection); err != nil {
		return KBRemoveResult{}, classifyBusy(fmt.Errorf("advance knowledge collection generation: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return KBRemoveResult{}, classifyBusy(err)
	}
	return KBRemoveResult{SourceID: source.ID, Chunks: source.Chunks}, nil
}

func truncateWAL(ctx context.Context, conn *sql.Conn) (bool, error) {
	var busy, logFrames, checkpointed int
	if err := conn.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		return false, classifyBusy(fmt.Errorf("truncate write-ahead log: %w", err))
	}
	return busy == 0, nil
}

func (s *sqliteKBStore) Check(ctx context.Context) (KBCheckReport, error) {
	if s.db == nil {
		return KBCheckReport{}, errNilArgument("db")
	}
	var report KBCheckReport
	for _, count := range []struct {
		target *int
		query  string
	}{
		{&report.Collections, `SELECT count(*) FROM kb_collection`},
		{&report.Sources, `SELECT count(*) FROM kb_source`},
		{&report.Chunks, `SELECT count(*) FROM kb_chunk`},
		{&report.IndexedChunks, `SELECT count(*) FROM kb_chunk_fts_docsize`},
	} {
		if err := s.db.QueryRowContext(ctx, count.query).Scan(count.target); err != nil {
			return KBCheckReport{}, classifyBusy(fmt.Errorf("count knowledge rows: %w", err))
		}
	}
	if report.Chunks != report.IndexedChunks {
		report.Problems = append(report.Problems, "chunk count differs from indexed row count")
	}
	scopeProblem, err := s.scopeMismatch(ctx)
	if err != nil {
		return KBCheckReport{}, err
	}
	if scopeProblem {
		report.Problems = append(report.Problems, "chunk scope does not match its collection")
	}
	damaged, err := s.indexDamaged(ctx)
	if err != nil {
		return KBCheckReport{}, err
	}
	if damaged {
		report.Problems = append(report.Problems, "full-text index does not match chunk content")
	}
	return report, nil
}

func (s *sqliteKBStore) scopeMismatch(ctx context.Context) (bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT s.collection, c.scope FROM kb_chunk c JOIN kb_source s ON s.id = c.source_id`)
	if err != nil {
		return false, classifyBusy(fmt.Errorf("list knowledge scopes: %w", err))
	}
	defer func() { _ = rows.Close() }()
	mismatch := false
	for rows.Next() {
		var collection, scope string
		if err := rows.Scan(&collection, &scope); err != nil {
			return false, fmt.Errorf("scan knowledge scope: %w", err)
		}
		if scope != KBScope(collection) {
			mismatch = true
		}
	}
	if err := rows.Err(); err != nil {
		return false, classifyBusy(fmt.Errorf("list knowledge scopes: %w", err))
	}
	return mismatch, nil
}

func (s *sqliteKBStore) indexDamaged(ctx context.Context) (bool, error) {
	_, err := s.db.ExecContext(ctx, `INSERT INTO kb_chunk_fts(kb_chunk_fts, rank) VALUES ('integrity-check', 1)`)
	if err == nil {
		return false, nil
	}
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) && sqliteErr.Code()&kbSQLitePrimary == kbSQLiteCorrupt {
		return true, nil
	}
	return false, classifyBusy(fmt.Errorf("check knowledge index: %w", err))
}
