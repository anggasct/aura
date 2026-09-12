package memory

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/anggasct/aura/internal/approval"
)

const (
	KindEventText = "event_text"
	KindSummary   = "summary"
)

const (
	defaultMaxDocuments = 10
	defaultTokenBudget  = 2000
	hardMaxDocuments    = 50
	hardMaxTokenBudget  = 8000
	maxQueryRunes       = 500
	maxContentBytes     = 1 << 16
)

type Event struct {
	ID        string
	SessionID string
	Sequence  uint64
	TurnID    string
	Author    string
	Kind      string
	Payload   json.RawMessage
	CreatedAt time.Time
}

type Document struct {
	ID           string
	OwnerID      string
	SessionID    string
	Kind         string
	FromSequence uint64
	ToSequence   uint64
	Content      string
	Trust        approval.TrustLabel
	CreatedAt    time.Time
	ExpiresAt    *time.Time
}

type Secrets interface {
	Contains(text string) bool
}

type DocumentStore interface {
	UpsertDocument(ctx context.Context, document *StoredDocument) (bool, error)
	ProjectionWatermark(ctx context.Context, sessionID string) (int64, bool, error)
	Search(ctx context.Context, query *StoredQuery) ([]StoredHit, error)
	DeleteSessionDocuments(ctx context.Context, sessionID string) error
	DeleteExpiredSummaries(ctx context.Context, now time.Time, limit int) (int, error)
}

type StoredDocument struct {
	ID           string
	OwnerID      string
	SessionID    string
	Kind         string
	FromSequence int64
	ToSequence   int64
	Content      string
	TrustLabel   string
	CreatedAt    time.Time
	ExpiresAt    *time.Time
}

type StoredQuery struct {
	OwnerID   string
	SessionID string
	Terms     string
	Before    time.Time
	Limit     int
}

type StoredHit struct {
	StoredDocument
	Rank float64
}

type RecallRequest struct {
	OwnerID      string
	SessionID    string
	Query        string
	MaxDocuments int
	TokenBudget  int
	Before       time.Time
}

type RecallDocument struct {
	ID           string
	SessionID    string
	FromSequence uint64
	ToSequence   uint64
	Content      string
	Trust        approval.TrustLabel
	Score        float64
	CreatedAt    time.Time
	ExpiresAt    *time.Time
}

type Config struct {
	MaxDocuments    int
	TokenBudget     int
	MaxContentBytes int
}

type Service struct {
	documents DocumentStore
	secrets   Secrets
	maxDocs   int
	tokens    int
	maxBytes  int
}

func NewService(documents DocumentStore, secrets Secrets, config Config) (*Service, error) {
	if documents == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "document store must not be nil")
	}
	maxDocs := config.MaxDocuments
	if maxDocs == 0 {
		maxDocs = defaultMaxDocuments
	}
	tokens := config.TokenBudget
	if tokens == 0 {
		tokens = defaultTokenBudget
	}
	maxBytes := config.MaxContentBytes
	if maxBytes == 0 {
		maxBytes = maxContentBytes
	}
	return &Service{documents: documents, secrets: secrets, maxDocs: maxDocs, tokens: tokens, maxBytes: maxBytes}, nil
}

func trustFor(author, kind string) (approval.TrustLabel, bool) {
	switch kind {
	case "tool.completed":
		return approval.TrustUntrustedExternal, true
	case "message.completed":
		return approval.TrustDerivedUntrusted, true
	case "adk_event":
		switch author {
		case "user":
			return approval.TrustOwnerInput, true
		case "model", "assistant":
			return approval.TrustDerivedUntrusted, true
		case "":
			return "", false
		default:
			return approval.TrustUntrustedExternal, true
		}
	default:
		return "", false
	}
}

func extractText(payload json.RawMessage) (string, bool) {
	var envelope struct {
		Content *struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
		Text    string `json:"text"`
		Partial bool   `json:"partial"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return "", false
	}
	if envelope.Partial {
		return "", false
	}
	if envelope.Content != nil {
		var combined strings.Builder
		for _, part := range envelope.Content.Parts {
			if part.Text != "" {
				combined.WriteString(part.Text)
			}
		}
		if combined.Len() > 0 {
			return combined.String(), true
		}
		return "", false
	}
	if strings.TrimSpace(envelope.Text) != "" {
		return envelope.Text, true
	}
	return "", false
}

func documentID(sessionID, kind string, from, to uint64) string {
	var numbers [16]byte
	binary.BigEndian.PutUint64(numbers[:8], from)
	binary.BigEndian.PutUint64(numbers[8:], to)
	sum := sha256.Sum256([]byte(sessionID + "|" + kind + "|" + hex.EncodeToString(numbers[:])))
	return "mem_" + hex.EncodeToString(sum[:])[:16]
}

func sequenceToDB(sequence uint64) (int64, error) {
	if sequence == 0 || sequence > math.MaxInt64 {
		return 0, Errorf(ErrorCodeInvalidArgument, "sequence is not valid")
	}
	return int64(sequence), nil
}

func sequenceFromDB(sequence int64) (uint64, error) {
	if sequence <= 0 {
		return 0, Errorf(ErrorCodeInvalidArgument, "sequence is not valid")
	}
	return uint64(sequence), nil
}

func cleanText(text string, maxBytes int) (string, bool) {
	if !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
		return "", false
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || len(trimmed) > maxBytes {
		return "", false
	}
	return trimmed, true
}

func (s *Service) ProjectEvent(ctx context.Context, ownerID string, event *Event) (Document, bool, error) {
	if ctx == nil {
		return Document{}, false, Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return Document{}, false, err
	}
	if strings.TrimSpace(ownerID) == "" {
		return Document{}, false, Errorf(ErrorCodeInvalidArgument, "owner must not be empty")
	}
	if event == nil {
		return Document{}, false, Errorf(ErrorCodeInvalidArgument, "event must not be nil")
	}
	if event.SessionID == "" {
		return Document{}, false, Errorf(ErrorCodeInvalidArgument, "event session must not be empty")
	}
	from, err := sequenceToDB(event.Sequence)
	if err != nil {
		return Document{}, false, err
	}
	trust, indexable := trustFor(event.Author, event.Kind)
	if !indexable {
		return Document{}, false, nil
	}
	text, ok := extractText(event.Payload)
	if !ok {
		return Document{}, false, nil
	}
	content, ok := cleanText(text, s.maxBytes)
	if !ok {
		return Document{}, false, nil
	}
	if s.secrets != nil && s.secrets.Contains(content) {
		return Document{}, false, nil
	}
	createdAt := event.CreatedAt.UTC()
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	document := Document{
		ID:           documentID(event.SessionID, KindEventText, event.Sequence, event.Sequence),
		OwnerID:      ownerID,
		SessionID:    event.SessionID,
		Kind:         KindEventText,
		FromSequence: event.Sequence,
		ToSequence:   event.Sequence,
		Content:      content,
		Trust:        trust,
		CreatedAt:    createdAt,
	}
	_, err = s.documents.UpsertDocument(ctx, &StoredDocument{
		ID:           document.ID,
		OwnerID:      document.OwnerID,
		SessionID:    document.SessionID,
		Kind:         document.Kind,
		FromSequence: from,
		ToSequence:   from,
		Content:      document.Content,
		TrustLabel:   string(document.Trust),
		CreatedAt:    document.CreatedAt,
	})
	if err != nil {
		return Document{}, false, err
	}
	return document, true, nil
}

func estimateTokens(content string) int {
	if tokens := len(content) / 4; tokens > 1 {
		return tokens
	}
	return 1
}

func (s *Service) Recall(ctx context.Context, req *RecallRequest) ([]RecallDocument, error) {
	if ctx == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "recall request must not be nil")
	}
	if strings.TrimSpace(req.OwnerID) == "" || strings.TrimSpace(req.SessionID) == "" {
		return nil, Errorf(ErrorCodeQueryInvalid, "recall owner and session must not be empty")
	}
	if len([]rune(req.Query)) == 0 || len([]rune(req.Query)) > maxQueryRunes {
		return nil, Errorf(ErrorCodeQueryInvalid, "recall query is not within bounds")
	}
	maxDocs := req.MaxDocuments
	if maxDocs == 0 {
		maxDocs = s.maxDocs
	}
	tokens := req.TokenBudget
	if tokens == 0 {
		tokens = s.tokens
	}
	if maxDocs < 0 || maxDocs > hardMaxDocuments || tokens <= 0 || tokens > hardMaxTokenBudget {
		return nil, Errorf(ErrorCodeBudgetInvalid, "recall budget is not within bounds")
	}
	hits, err := s.documents.Search(ctx, &StoredQuery{
		OwnerID:   req.OwnerID,
		SessionID: req.SessionID,
		Terms:     req.Query,
		Before:    req.Before,
		Limit:     maxDocs,
	})
	if err != nil {
		return nil, err
	}
	documents := make([]RecallDocument, 0, len(hits))
	used := 0
	for i := range hits {
		estimate := estimateTokens(hits[i].Content)
		if used+estimate > tokens {
			break
		}
		used += estimate
		from, err := sequenceFromDB(hits[i].FromSequence)
		if err != nil {
			return nil, err
		}
		to, err := sequenceFromDB(hits[i].ToSequence)
		if err != nil {
			return nil, err
		}
		documents = append(documents, RecallDocument{
			ID:           hits[i].ID,
			SessionID:    hits[i].SessionID,
			FromSequence: from,
			ToSequence:   to,
			Content:      hits[i].Content,
			Trust:        approval.TrustLabel(hits[i].TrustLabel),
			Score:        hits[i].Rank,
			CreatedAt:    hits[i].CreatedAt,
			ExpiresAt:    hits[i].ExpiresAt,
		})
	}
	return documents, nil
}

func (s *Service) Watermark(ctx context.Context, sessionID string) (mark uint64, found bool, err error) {
	if ctx == nil {
		return 0, false, Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if strings.TrimSpace(sessionID) == "" {
		return 0, false, Errorf(ErrorCodeInvalidArgument, "session id must not be empty")
	}
	raw, found, err := s.documents.ProjectionWatermark(ctx, sessionID)
	if err != nil {
		return 0, false, err
	}
	if !found {
		return 0, false, nil
	}
	mark, err = sequenceFromDB(raw)
	if err != nil {
		return 0, false, err
	}
	return mark, true, nil
}

func (s *Service) RebuildSession(ctx context.Context, ownerID, sessionID string, events []*Event) (int, error) {
	if ctx == nil {
		return 0, Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if strings.TrimSpace(ownerID) == "" || strings.TrimSpace(sessionID) == "" {
		return 0, Errorf(ErrorCodeInvalidArgument, "owner and session must not be empty")
	}
	if err := s.documents.DeleteSessionDocuments(ctx, sessionID); err != nil {
		return 0, err
	}
	count := 0
	for _, event := range events {
		if err := ctx.Err(); err != nil {
			return count, err
		}
		_, indexed, err := s.ProjectEvent(ctx, ownerID, event)
		if err != nil {
			return count, err
		}
		if indexed {
			count++
		}
	}
	return count, nil
}

func (s *Service) PurgeExpired(ctx context.Context, now time.Time, limit int) (int, error) {
	if ctx == nil {
		return 0, Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if limit <= 0 {
		return 0, Errorf(ErrorCodeInvalidArgument, "purge limit must be positive")
	}
	return s.documents.DeleteExpiredSummaries(ctx, now.UTC(), limit)
}
