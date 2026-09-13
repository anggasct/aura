package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const (
	MemoryKindEventText = "event_text"
	MemoryKindSummary   = "summary"
)

type MemoryDocument struct {
	ID            string
	OwnerID       string
	SessionID     string
	Kind          string
	FromSequence  int64
	ToSequence    int64
	Content       string
	TrustLabel    string
	PromptVersion string
	ModelProtocol string
	ModelName     string
	CreatedAt     time.Time
	ExpiresAt     *time.Time
}

type MemoryHit struct {
	MemoryDocument
	Rank float64
}

type MemoryQuery struct {
	OwnerID   string
	SessionID string
	Terms     string
	Before    time.Time
	Limit     int
}

type MemoryStore interface {
	UpsertDocument(ctx context.Context, document *MemoryDocument) (bool, error)
	ProjectionWatermark(ctx context.Context, sessionID string) (int64, bool, error)
	Search(ctx context.Context, query *MemoryQuery) ([]MemoryHit, error)
	DeleteSessionDocuments(ctx context.Context, sessionID string) error
	DeleteExpiredSummaries(ctx context.Context, now time.Time, limit int) (int, error)
}

type sqliteMemoryStore struct {
	db *sql.DB
}

func NewMemoryStore(db *sql.DB) MemoryStore {
	return &sqliteMemoryStore{db: db}
}

func validMemoryKind(kind string) bool {
	return kind == MemoryKindEventText || kind == MemoryKindSummary
}

func isFTSWordChar(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r > 127
}

func sanitizeFTSQuery(raw string) (string, error) {
	if len([]rune(raw)) > 500 {
		return "", Errorf(ErrorCodeInvalidArgument, "search query exceeds length bounds")
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
		return "", Errorf(ErrorCodeInvalidArgument, "search query carries no searchable terms")
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
	return sql.NullString{String: formatTime(value.UTC()), Valid: true}
}

func (s *sqliteMemoryStore) UpsertDocument(ctx context.Context, document *MemoryDocument) (bool, error) {
	if s.db == nil {
		return false, errNilArgument("db")
	}
	if document == nil {
		return false, errNilArgument("document")
	}
	if document.ID == "" || document.OwnerID == "" || document.SessionID == "" {
		return false, Errorf(ErrorCodeInvalidArgument, "memory document identity must not be empty")
	}
	if !validMemoryKind(document.Kind) {
		return false, Errorf(ErrorCodeMemoryInvalid, "memory document kind is not valid")
	}
	if document.FromSequence <= 0 || document.ToSequence < document.FromSequence {
		return false, Errorf(ErrorCodeMemoryInvalid, "memory document sequence range is not valid")
	}
	if document.Content == "" {
		return false, Errorf(ErrorCodeInvalidArgument, "memory document content must not be empty")
	}
	if document.TrustLabel == "" {
		return false, Errorf(ErrorCodeInvalidArgument, "memory trust label must not be empty")
	}
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO memory_document (
			id, owner_id, session_id, kind, source_from_sequence, source_to_sequence,
			content, trust_label, prompt_version, model_protocol, model_name,
			created_at, expires_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id, kind, source_from_sequence, source_to_sequence, prompt_version) DO NOTHING`,
		document.ID, document.OwnerID, document.SessionID, document.Kind,
		document.FromSequence, document.ToSequence, document.Content, document.TrustLabel,
		document.PromptVersion, nullMemoryText(document.ModelProtocol), nullMemoryText(document.ModelName),
		formatTime(document.CreatedAt.UTC()), nullMemoryTime(document.ExpiresAt),
	)
	if err != nil {
		return false, classifyBusy(fmt.Errorf("upsert memory document: %w", err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, classifyBusy(fmt.Errorf("upsert memory document: %w", err))
	}
	return affected > 0, nil
}

func (s *sqliteMemoryStore) ProjectionWatermark(ctx context.Context, sessionID string) (mark int64, found bool, err error) {
	if s.db == nil {
		return 0, false, errNilArgument("db")
	}
	if sessionID == "" {
		return 0, false, Errorf(ErrorCodeInvalidArgument, "session id must not be empty")
	}
	var watermark sql.NullInt64
	err = s.db.QueryRowContext(ctx,
		`SELECT MAX(source_to_sequence) FROM memory_document WHERE session_id = ?`,
		sessionID).Scan(&watermark)
	if err != nil {
		return 0, false, classifyBusy(fmt.Errorf("load projection watermark: %w", err))
	}
	if !watermark.Valid {
		return 0, false, nil
	}
	return watermark.Int64, true, nil
}

const selectMemoryHitColumns = `
	d.id, d.owner_id, d.session_id, d.kind, d.source_from_sequence, d.source_to_sequence,
	d.content, d.trust_label, d.prompt_version, d.model_protocol, d.model_name,
	d.created_at, d.expires_at, bm25(memory_document_fts)
`

func (s *sqliteMemoryStore) Search(ctx context.Context, query *MemoryQuery) ([]MemoryHit, error) {
	if s.db == nil {
		return nil, errNilArgument("db")
	}
	if query == nil {
		return nil, errNilArgument("query")
	}
	if query.OwnerID == "" {
		return nil, Errorf(ErrorCodeInvalidArgument, "search owner must not be empty")
	}
	if query.Limit <= 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "search limit must be positive")
	}
	match, err := sanitizeFTSQuery(query.Terms)
	if err != nil {
		return nil, err
	}
	sqlQuery := `SELECT ` + selectMemoryHitColumns + ` FROM memory_document_fts
		JOIN memory_document d ON d.rowid = memory_document_fts.rowid
		WHERE memory_document_fts MATCH ? AND d.owner_id = ?`
	args := []any{match, query.OwnerID}
	if query.SessionID != "" {
		sqlQuery += ` AND d.session_id = ?`
		args = append(args, query.SessionID)
	}
	if !query.Before.IsZero() {
		sqlQuery += ` AND d.created_at <= ?`
		args = append(args, formatTime(query.Before.UTC()))
	}
	now := time.Now().UTC()
	sqlQuery += ` AND (d.expires_at IS NULL OR d.expires_at > ?)`
	args = append(args, formatTime(now))
	sqlQuery += ` ORDER BY bm25(memory_document_fts), d.id LIMIT ?`
	args = append(args, query.Limit)
	rows, err := s.db.QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("search memory documents: %w", err))
	}
	defer func() { _ = rows.Close() }()
	hits := []MemoryHit{}
	for rows.Next() {
		hit, err := scanMemoryHit(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		hits = append(hits, hit)
	}
	if err := rows.Err(); err != nil {
		return nil, classifyBusy(fmt.Errorf("search memory documents: %w", err))
	}
	return hits, nil
}

func scanMemoryHit(rows *sql.Rows) (MemoryHit, error) {
	var hit MemoryHit
	var createdAtRaw string
	var expiresAtRaw sql.NullString
	var promptVersion, modelProtocol, modelName sql.NullString
	err := rows.Scan(
		&hit.ID, &hit.OwnerID, &hit.SessionID, &hit.Kind,
		&hit.FromSequence, &hit.ToSequence, &hit.Content, &hit.TrustLabel,
		&promptVersion, &modelProtocol, &modelName,
		&createdAtRaw, &expiresAtRaw, &hit.Rank,
	)
	if err != nil {
		return MemoryHit{}, classifyBusy(fmt.Errorf("scan memory hit: %w", err))
	}
	createdAt, err := parseTime(createdAtRaw)
	if err != nil {
		return MemoryHit{}, Errorf(ErrorCodeMemoryInvalid, "memory created_at is not valid")
	}
	hit.CreatedAt = createdAt
	hit.PromptVersion = promptVersion.String
	hit.ModelProtocol = modelProtocol.String
	hit.ModelName = modelName.String
	if expiresAtRaw.Valid {
		expiresAt, err := parseTime(expiresAtRaw.String)
		if err != nil {
			return MemoryHit{}, Errorf(ErrorCodeMemoryInvalid, "memory expires_at is not valid")
		}
		hit.ExpiresAt = &expiresAt
	}
	return hit, nil
}

func (s *sqliteMemoryStore) DeleteSessionDocuments(ctx context.Context, sessionID string) error {
	if s.db == nil {
		return errNilArgument("db")
	}
	if sessionID == "" {
		return Errorf(ErrorCodeInvalidArgument, "session id must not be empty")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM memory_document WHERE session_id = ?`, sessionID); err != nil {
		return classifyBusy(fmt.Errorf("delete session documents: %w", err))
	}
	return nil
}

func (s *sqliteMemoryStore) DeleteExpiredSummaries(ctx context.Context, now time.Time, limit int) (int, error) {
	if s.db == nil {
		return 0, errNilArgument("db")
	}
	if limit <= 0 {
		return 0, Errorf(ErrorCodeInvalidArgument, "delete limit must be positive")
	}
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM memory_document WHERE id IN (
			SELECT id FROM memory_document
			WHERE kind = ? AND expires_at IS NOT NULL AND expires_at <= ?
			ORDER BY expires_at, id LIMIT ?
		)`, MemoryKindSummary, formatTime(now.UTC()), limit)
	if err != nil {
		return 0, classifyBusy(fmt.Errorf("delete expired summaries: %w", err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, classifyBusy(fmt.Errorf("delete expired summaries: %w", err))
	}
	return int(affected), nil
}
