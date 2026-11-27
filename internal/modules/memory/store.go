package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/turn"
)

const (
	maxSessionListLimit = 500
	maxMemoryListLimit  = 500
)

const selectMemoryHitColumns = `
	d.id, d.owner_id, d.session_id, d.kind, d.source_from_sequence, d.source_to_sequence,
	d.content, d.trust_label, d.prompt_version, d.model_protocol, d.model_name,
	d.created_at, d.expires_at, d.agent_id, bm25(memory_document_fts)
`

const selectMemoryDocumentColumns = `
	d.id, d.owner_id, d.session_id, d.kind, d.source_from_sequence, d.source_to_sequence,
	d.content, d.trust_label, d.prompt_version, d.model_protocol, d.model_name,
	d.created_at, d.expires_at, d.agent_id
`

type SessionReader interface {
	Get(ctx context.Context, id string) (turn.Session, error)
}

type SQLiteStore struct {
	db       *sql.DB
	sessions SessionReader
}

func NewSQLiteStore(db *sql.DB, sessions SessionReader) (*SQLiteStore, error) {
	if db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "db must not be nil")
	}
	if sessions == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "sessions must not be nil")
	}
	return &SQLiteStore{db: db, sessions: sessions}, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func validMemoryKind(kind string) bool {
	return kind == KindEventText || kind == KindSummary
}

func isFTSWordChar(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r > 127
}

func sanitizeFTSQuery(raw string) (string, error) {
	if len([]rune(raw)) > 500 {
		return "", fault.New(ErrorCodeInvalidArgument, "search query exceeds length bounds")
	}
	terms := strings.FieldsFunc(raw, func(r rune) bool { return !isFTSWordChar(r) })
	cleaned := make([]string, 0, len(terms))
	for _, term := range terms {
		term = strings.TrimSpace(term)
		if term == "" || len([]rune(term)) > 64 {
			continue
		}
		cleaned = append(cleaned, `"`+strings.ReplaceAll(term, `"`, `""`)+`"`)
		if len(cleaned) >= 32 {
			break
		}
	}
	if len(cleaned) == 0 {
		return "", fault.New(ErrorCodeInvalidArgument, "search query carries no searchable terms")
	}
	return strings.Join(cleaned, " "), nil
}

func nullMemoryText(value string) sql.NullString {
	if value == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: value, Valid: true}
}

func nullMemoryTime(value *time.Time) sql.NullString {
	if value == nil || value.IsZero() {
		return sql.NullString{}
	}
	return sql.NullString{String: sqlite.FormatTime(value.UTC()), Valid: true}
}

func (s *SQLiteStore) UpsertDocument(ctx context.Context, document *StoredDocument) (bool, error) {
	if s == nil || s.db == nil {
		return false, fault.New(ErrorCodeInvalidArgument, "db must not be nil")
	}
	if s.sessions == nil {
		return false, fault.New(ErrorCodeInvalidArgument, "sessions must not be nil")
	}
	if document == nil {
		return false, fault.New(ErrorCodeInvalidArgument, "document must not be nil")
	}
	if document.ID == "" || document.OwnerID == "" || document.SessionID == "" {
		return false, fault.New(ErrorCodeInvalidArgument, "memory document identity must not be empty")
	}
	if !validMemoryKind(document.Kind) {
		return false, fault.New(ErrorCodeMemoryInvalid, "memory document kind is not valid")
	}
	if document.FromSequence <= 0 || document.ToSequence < document.FromSequence {
		return false, fault.New(ErrorCodeMemoryInvalid, "memory document sequence range is not valid")
	}
	if document.Content == "" {
		return false, fault.New(ErrorCodeInvalidArgument, "memory document content must not be empty")
	}
	if document.TrustLabel == "" {
		return false, fault.New(ErrorCodeInvalidArgument, "memory trust label must not be empty")
	}
	var agentID sql.NullString
	session, err := s.sessions.Get(ctx, document.SessionID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("load memory session: %w", err)
		}
	} else {
		agentID = nullMemoryText(session.AgentID)
	}
	result, err := sqlite.Conn(ctx, s.db).ExecContext(ctx,
		`INSERT INTO memory_document (
			id, owner_id, session_id, kind, source_from_sequence, source_to_sequence,
			content, trust_label, prompt_version, model_protocol, model_name,
			created_at, expires_at, agent_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id, kind, source_from_sequence, source_to_sequence, prompt_version) DO NOTHING`,
		document.ID, document.OwnerID, document.SessionID, document.Kind,
		document.FromSequence, document.ToSequence, document.Content, document.TrustLabel,
		document.PromptVersion, nullMemoryText(document.ModelProtocol), nullMemoryText(document.ModelName),
		sqlite.FormatTime(document.CreatedAt.UTC()), nullMemoryTime(document.ExpiresAt), agentID,
	)
	if err != nil {
		return false, sqlite.Classify(fmt.Errorf("upsert memory document: %w", err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, sqlite.Classify(fmt.Errorf("upsert memory document: %w", err))
	}
	return affected > 0, nil
}

func (s *SQLiteStore) ProjectionWatermark(ctx context.Context, sessionID string) (mark int64, found bool, err error) {
	if s == nil || s.db == nil {
		return 0, false, fault.New(ErrorCodeInvalidArgument, "db must not be nil")
	}
	if sessionID == "" {
		return 0, false, fault.New(ErrorCodeInvalidArgument, "session id must not be empty")
	}
	var watermark sql.NullInt64
	err = sqlite.Conn(ctx, s.db).QueryRowContext(ctx,
		`SELECT MAX(source_to_sequence) FROM memory_document WHERE session_id = ?`,
		sessionID).Scan(&watermark)
	if err != nil {
		return 0, false, sqlite.Classify(fmt.Errorf("load projection watermark: %w", err))
	}
	if !watermark.Valid {
		return 0, false, nil
	}
	return watermark.Int64, true, nil
}

func (s *SQLiteStore) Search(ctx context.Context, query *StoredQuery) ([]StoredHit, error) {
	if s == nil || s.db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "db must not be nil")
	}
	if query == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "query must not be nil")
	}
	if query.OwnerID == "" {
		return nil, fault.New(ErrorCodeInvalidArgument, "search owner must not be empty")
	}
	if query.Limit <= 0 {
		return nil, fault.New(ErrorCodeInvalidArgument, "search limit must be positive")
	}
	match, err := sanitizeFTSQuery(query.Terms)
	if err != nil {
		return nil, err
	}
	sqlQuery := `SELECT ` + selectMemoryHitColumns + ` FROM memory_document_fts
		JOIN memory_document d ON d.rowid = memory_document_fts.rowid
		WHERE memory_document_fts MATCH ? AND d.owner_id = ?`
	args := []any{match, query.OwnerID}
	if len(query.AgentIDs) > 0 {
		readable, err := json.Marshal(query.AgentIDs)
		if err != nil {
			return nil, fault.New(ErrorCodeInvalidArgument, "search personas are not encodable")
		}
		sqlQuery += ` AND d.agent_id IN (SELECT value FROM json_each(?))`
		args = append(args, string(readable))
	}
	if query.SessionID != "" {
		sqlQuery += ` AND d.session_id = ?`
		args = append(args, query.SessionID)
	}
	if !query.Before.IsZero() {
		sqlQuery += ` AND d.created_at <= ?`
		args = append(args, sqlite.FormatTime(query.Before.UTC()))
	}
	now := time.Now().UTC()
	sqlQuery += ` AND (d.expires_at IS NULL OR d.expires_at > ?)`
	args = append(args, sqlite.FormatTime(now))
	sqlQuery += ` ORDER BY bm25(memory_document_fts), d.id LIMIT ?`
	args = append(args, query.Limit)
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("search memory documents: %w", err))
	}
	defer func() { _ = rows.Close() }()
	hits := []StoredHit{}
	for rows.Next() {
		hit, err := scanMemoryHit(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		hits = append(hits, hit)
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("search memory documents: %w", err))
	}
	return hits, nil
}

func scanMemoryHit(rows *sql.Rows) (StoredHit, error) {
	var hit StoredHit
	var createdAtRaw string
	var expiresAtRaw sql.NullString
	var promptVersion, modelProtocol, modelName sql.NullString
	err := rows.Scan(
		&hit.ID, &hit.OwnerID, &hit.SessionID, &hit.Kind,
		&hit.FromSequence, &hit.ToSequence, &hit.Content, &hit.TrustLabel,
		&promptVersion, &modelProtocol, &modelName,
		&createdAtRaw, &expiresAtRaw, &hit.AgentID, &hit.Rank,
	)
	if err != nil {
		return StoredHit{}, sqlite.Classify(fmt.Errorf("scan memory hit: %w", err))
	}
	createdAt, err := sqlite.ParseTime(createdAtRaw)
	if err != nil {
		return StoredHit{}, fault.New(ErrorCodeMemoryInvalid, "memory created_at is not valid")
	}
	hit.CreatedAt = createdAt
	hit.PromptVersion = promptVersion.String
	hit.ModelProtocol = modelProtocol.String
	hit.ModelName = modelName.String
	if expiresAtRaw.Valid {
		expiresAt, err := sqlite.ParseTime(expiresAtRaw.String)
		if err != nil {
			return StoredHit{}, fault.New(ErrorCodeMemoryInvalid, "memory expires_at is not valid")
		}
		hit.ExpiresAt = &expiresAt
	}
	return hit, nil
}

func (s *SQLiteStore) DeleteSessionDocuments(ctx context.Context, sessionID string) error {
	if s == nil || s.db == nil {
		return fault.New(ErrorCodeInvalidArgument, "db must not be nil")
	}
	if sessionID == "" {
		return fault.New(ErrorCodeInvalidArgument, "session id must not be empty")
	}
	if _, err := sqlite.Conn(ctx, s.db).ExecContext(ctx, `DELETE FROM memory_document WHERE session_id = ?`, sessionID); err != nil {
		return sqlite.Classify(fmt.Errorf("delete session documents: %w", err))
	}
	return nil
}

func (s *SQLiteStore) DeleteExpiredSummaries(ctx context.Context, now time.Time, limit int) (int, error) {
	if s == nil || s.db == nil {
		return 0, fault.New(ErrorCodeInvalidArgument, "db must not be nil")
	}
	if limit <= 0 {
		return 0, fault.New(ErrorCodeInvalidArgument, "delete limit must be positive")
	}
	result, err := sqlite.Conn(ctx, s.db).ExecContext(ctx,
		`DELETE FROM memory_document WHERE id IN (
			SELECT id FROM memory_document
			WHERE kind = ? AND expires_at IS NOT NULL AND expires_at <= ?
			ORDER BY expires_at, id LIMIT ?
		)`, KindSummary, sqlite.FormatTime(now.UTC()), limit)
	if err != nil {
		return 0, sqlite.Classify(fmt.Errorf("delete expired summaries: %w", err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, sqlite.Classify(fmt.Errorf("delete expired summaries: %w", err))
	}
	return int(affected), nil
}

func (s *SQLiteStore) ListSessionDocuments(ctx context.Context, ownerID, sessionID string, limit int) ([]StoredDocument, error) {
	if s == nil || s.db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "db must not be nil")
	}
	if ownerID == "" || sessionID == "" {
		return nil, fault.New(ErrorCodeInvalidArgument, "memory owner and session must not be empty")
	}
	if limit <= 0 || limit > maxMemoryListLimit {
		return nil, fault.New(ErrorCodeInvalidArgument, "memory list limit is out of bounds")
	}
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx,
		`SELECT `+selectMemoryDocumentColumns+` FROM memory_document d
		 WHERE d.owner_id = ? AND d.session_id = ?
		 ORDER BY d.source_from_sequence, d.id LIMIT ?`,
		ownerID, sessionID, limit)
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list session documents: %w", err))
	}
	defer func() { _ = rows.Close() }()
	documents := make([]StoredDocument, 0, limit)
	for rows.Next() {
		document, err := scanMemoryDocument(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		documents = append(documents, document)
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list session documents: %w", err))
	}
	return documents, nil
}

func (s *SQLiteStore) ListSessionDocumentsByKind(ctx context.Context, ownerID, sessionID, kind string, limit int) ([]StoredDocument, error) {
	if s == nil || s.db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "db must not be nil")
	}
	if ownerID == "" || sessionID == "" {
		return nil, fault.New(ErrorCodeInvalidArgument, "list owner and session must not be empty")
	}
	if !validMemoryKind(kind) {
		return nil, fault.New(ErrorCodeMemoryInvalid, "memory document kind is not valid")
	}
	if limit <= 0 || limit > maxSessionListLimit {
		return nil, fault.New(ErrorCodeInvalidArgument, "list limit is not within bounds")
	}
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx,
		`SELECT id, owner_id, session_id, kind, source_from_sequence, source_to_sequence,
			content, trust_label, prompt_version, model_protocol, model_name,
			created_at, expires_at, agent_id
		FROM memory_document
		WHERE owner_id = ? AND session_id = ? AND kind = ?
			AND (expires_at IS NULL OR expires_at > ?)
		ORDER BY source_from_sequence ASC, source_to_sequence ASC, id ASC
		LIMIT ?`,
		ownerID, sessionID, kind, sqlite.FormatTime(time.Now().UTC()), limit)
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list session documents by kind: %w", err))
	}
	defer func() { _ = rows.Close() }()
	documents := []StoredDocument{}
	for rows.Next() {
		document, err := scanMemoryDocument(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		documents = append(documents, document)
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list session documents by kind: %w", err))
	}
	return documents, nil
}

func (s *SQLiteStore) ListSessionDocumentsByKindInRange(ctx context.Context, ownerID, sessionID, kind string, from, to int64, limit int) ([]StoredDocument, error) {
	if s == nil || s.db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "db must not be nil")
	}
	if ownerID == "" || sessionID == "" {
		return nil, fault.New(ErrorCodeInvalidArgument, "list owner and session must not be empty")
	}
	if !validMemoryKind(kind) {
		return nil, fault.New(ErrorCodeMemoryInvalid, "memory document kind is not valid")
	}
	if from > to {
		return nil, fault.New(ErrorCodeInvalidArgument, "list sequence range is not within bounds")
	}
	if limit <= 0 || limit > maxSessionListLimit {
		return nil, fault.New(ErrorCodeInvalidArgument, "list limit is not within bounds")
	}
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx,
		`SELECT id, owner_id, session_id, kind, source_from_sequence, source_to_sequence,
			content, trust_label, prompt_version, model_protocol, model_name,
			created_at, expires_at, agent_id
		FROM memory_document
		WHERE owner_id = ? AND session_id = ? AND kind = ?
			AND source_from_sequence >= ? AND source_to_sequence <= ?
			AND (expires_at IS NULL OR expires_at > ?)
		ORDER BY source_from_sequence ASC, source_to_sequence ASC, id ASC
		LIMIT ?`,
		ownerID, sessionID, kind, from, to, sqlite.FormatTime(time.Now().UTC()), limit)
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list session documents in range: %w", err))
	}
	defer func() { _ = rows.Close() }()
	documents := []StoredDocument{}
	for rows.Next() {
		document, err := scanMemoryDocument(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		documents = append(documents, document)
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list session documents in range: %w", err))
	}
	return documents, nil
}

func scanMemoryDocument(rows rowScanner) (StoredDocument, error) {
	var document StoredDocument
	var createdAtRaw string
	var expiresAtRaw sql.NullString
	var promptVersion, modelProtocol, modelName sql.NullString
	err := rows.Scan(
		&document.ID, &document.OwnerID, &document.SessionID, &document.Kind,
		&document.FromSequence, &document.ToSequence, &document.Content, &document.TrustLabel,
		&promptVersion, &modelProtocol, &modelName,
		&createdAtRaw, &expiresAtRaw, &document.AgentID,
	)
	if err != nil {
		return StoredDocument{}, sqlite.Classify(fmt.Errorf("scan memory document: %w", err))
	}
	createdAt, err := sqlite.ParseTime(createdAtRaw)
	if err != nil {
		return StoredDocument{}, fault.New(ErrorCodeMemoryInvalid, "memory created_at is not valid")
	}
	document.CreatedAt = createdAt
	document.PromptVersion = promptVersion.String
	document.ModelProtocol = modelProtocol.String
	document.ModelName = modelName.String
	if expiresAtRaw.Valid {
		expiresAt, err := sqlite.ParseTime(expiresAtRaw.String)
		if err != nil {
			return StoredDocument{}, fault.New(ErrorCodeMemoryInvalid, "memory expires_at is not valid")
		}
		document.ExpiresAt = &expiresAt
	}
	return document, nil
}

func (s *SQLiteStore) DeleteSessionSummaries(ctx context.Context, ownerID, sessionID string) (int, error) {
	if s == nil || s.db == nil {
		return 0, fault.New(ErrorCodeInvalidArgument, "db must not be nil")
	}
	if ownerID == "" || sessionID == "" {
		return 0, fault.New(ErrorCodeInvalidArgument, "delete owner and session must not be empty")
	}
	result, err := sqlite.Conn(ctx, s.db).ExecContext(ctx,
		`DELETE FROM memory_document WHERE owner_id = ? AND session_id = ? AND kind = ?`,
		ownerID, sessionID, KindSummary)
	if err != nil {
		return 0, sqlite.Classify(fmt.Errorf("delete session summaries: %w", err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, sqlite.Classify(fmt.Errorf("delete session summaries: %w", err))
	}
	return int(affected), nil
}

func (s *SQLiteStore) DeleteOtherSummaries(ctx context.Context, ownerID, sessionID, keepID string) (int, error) {
	if s == nil || s.db == nil {
		return 0, fault.New(ErrorCodeInvalidArgument, "db must not be nil")
	}
	if ownerID == "" || sessionID == "" || keepID == "" {
		return 0, fault.New(ErrorCodeInvalidArgument, "delete owner, session and keep id must not be empty")
	}
	result, err := sqlite.Conn(ctx, s.db).ExecContext(ctx,
		`DELETE FROM memory_document WHERE owner_id = ? AND session_id = ? AND kind = ? AND id != ?`,
		ownerID, sessionID, KindSummary, keepID)
	if err != nil {
		return 0, sqlite.Classify(fmt.Errorf("delete other summaries: %w", err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, sqlite.Classify(fmt.Errorf("delete other summaries: %w", err))
	}
	return int(affected), nil
}

func (s *SQLiteStore) GetDocument(ctx context.Context, id string) (StoredDocument, bool, error) {
	if s == nil || s.db == nil {
		return StoredDocument{}, false, fault.New(ErrorCodeInvalidArgument, "db must not be nil")
	}
	if id == "" {
		return StoredDocument{}, false, fault.New(ErrorCodeInvalidArgument, "document id must not be empty")
	}
	row := sqlite.Conn(ctx, s.db).QueryRowContext(ctx,
		`SELECT id, owner_id, session_id, kind, source_from_sequence, source_to_sequence,
			content, trust_label, prompt_version, model_protocol, model_name,
			created_at, expires_at, agent_id FROM memory_document WHERE id = ?`, id)
	document, err := scanMemoryDocument(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return StoredDocument{}, false, nil
		}
		return StoredDocument{}, false, err
	}
	return document, true, nil
}

func (s *SQLiteStore) ListOwnerDocuments(ctx context.Context, ownerID string, limit int) ([]StoredDocument, error) {
	if s == nil || s.db == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "db must not be nil")
	}
	if ownerID == "" {
		return nil, fault.New(ErrorCodeInvalidArgument, "memory owner must not be empty")
	}
	if limit <= 0 || limit > maxMemoryListLimit {
		return nil, fault.New(ErrorCodeInvalidArgument, "memory list limit is out of bounds")
	}
	rows, err := sqlite.Conn(ctx, s.db).QueryContext(ctx,
		`SELECT `+selectMemoryDocumentColumns+` FROM memory_document d
		 WHERE d.owner_id = ?
		 ORDER BY d.session_id, d.source_from_sequence, d.id LIMIT ?`,
		ownerID, limit)
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list owner documents: %w", err))
	}
	defer func() { _ = rows.Close() }()
	documents := make([]StoredDocument, 0, limit)
	for rows.Next() {
		document, err := scanMemoryDocument(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		documents = append(documents, document)
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("list owner documents: %w", err))
	}
	return documents, nil
}

func (s *SQLiteStore) CountSessionDocuments(ctx context.Context, sessionID string) (int, error) {
	if s == nil || s.db == nil {
		return 0, fault.New(ErrorCodeInvalidArgument, "db must not be nil")
	}
	if sessionID == "" {
		return 0, fault.New(ErrorCodeInvalidArgument, "session id must not be empty")
	}
	var total int64
	if err := sqlite.Conn(ctx, s.db).QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memory_document WHERE session_id = ?`, sessionID).Scan(&total); err != nil {
		return 0, sqlite.Classify(fmt.Errorf("count session documents: %w", err))
	}
	if total > math.MaxInt {
		return 0, fault.New(ErrorCodeMemoryInvalid, "session document count is out of bounds")
	}
	return int(total), nil
}

func (s *SQLiteStore) DeleteDocumentsByIDsOwned(ctx context.Context, ownerID string, ids []string) (int, error) {
	if s == nil || s.db == nil {
		return 0, fault.New(ErrorCodeInvalidArgument, "db must not be nil")
	}
	if ownerID == "" {
		return 0, fault.New(ErrorCodeInvalidArgument, "memory owner must not be empty")
	}
	if len(ids) == 0 {
		return 0, nil
	}
	if len(ids) > maxMemoryListLimit {
		return 0, fault.New(ErrorCodeInvalidArgument, "memory delete batch is out of bounds")
	}
	for i := range ids {
		if ids[i] == "" {
			return 0, fault.New(ErrorCodeInvalidArgument, "memory document id must not be empty")
		}
	}
	deleted := 0
	err := sqlite.RunInTx(ctx, s.db, func(txCtx context.Context) error {
		conn := sqlite.Conn(txCtx, s.db)
		for i := range ids {
			result, err := conn.ExecContext(txCtx,
				`DELETE FROM memory_document WHERE owner_id = ? AND id = ?`, ownerID, ids[i])
			if err != nil {
				return sqlite.Classify(fmt.Errorf("delete owned documents: %w", err))
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return sqlite.Classify(fmt.Errorf("delete owned documents: %w", err))
			}
			if affected > math.MaxInt-int64(deleted) {
				return fault.New(ErrorCodeMemoryInvalid, "delete count is out of bounds")
			}
			deleted += int(affected)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}
