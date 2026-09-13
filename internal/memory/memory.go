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

const (
	defaultSummaryPromptVersion = "memory-summary-v1"
	defaultSummaryTTL           = 720 * time.Hour
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
	ID            string
	SessionID     string
	FromSequence  uint64
	ToSequence    uint64
	Content       string
	Trust         approval.TrustLabel
	PromptVersion string
	ModelProtocol string
	ModelName     string
	Score         float64
	CreatedAt     time.Time
	ExpiresAt     *time.Time
}

type ProvenanceRef struct {
	DocumentID   string
	SessionID    string
	FromSequence uint64
	ToSequence   uint64
}

type RecallContext struct {
	Query      string
	Documents  []RecallDocument
	Summary    string
	Provenance []ProvenanceRef
	Trust      approval.TrustLabel
}

type SummaryModel struct {
	Protocol         string
	Name             string
	Tokenizer        string
	ContextTokens    int
	StructuredOutput bool
}

type SummaryPrompt struct {
	Version   string
	Query     string
	Documents []RecallDocument
	MaxTokens int
}

type Summarizer interface {
	Summarize(ctx context.Context, prompt *SummaryPrompt) (string, error)
}

type Config struct {
	MaxDocuments         int
	TokenBudget          int
	MaxContentBytes      int
	SummaryPromptVersion string
	SummaryTTL           time.Duration
}

type Service struct {
	documents     DocumentStore
	secrets       Secrets
	maxDocs       int
	tokens        int
	maxBytes      int
	summaryPrompt string
	summaryTTL    time.Duration
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
	summaryPrompt := config.SummaryPromptVersion
	if summaryPrompt == "" {
		summaryPrompt = defaultSummaryPromptVersion
	}
	summaryTTL := config.SummaryTTL
	if summaryTTL == 0 {
		summaryTTL = defaultSummaryTTL
	}
	return &Service{
		documents:     documents,
		secrets:       secrets,
		maxDocs:       maxDocs,
		tokens:        tokens,
		maxBytes:      maxBytes,
		summaryPrompt: summaryPrompt,
		summaryTTL:    summaryTTL,
	}, nil
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

func documentID(sessionID, kind string, from, to uint64, promptVersion string) string {
	var numbers [16]byte
	binary.BigEndian.PutUint64(numbers[:8], from)
	binary.BigEndian.PutUint64(numbers[8:], to)
	sum := sha256.Sum256([]byte(sessionID + "|" + kind + "|" + promptVersion + "|" + hex.EncodeToString(numbers[:])))
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
		ID:           documentID(event.SessionID, KindEventText, event.Sequence, event.Sequence, ""),
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
			return nil, Errorf(ErrorCodeProjectionCorrupt, "projected document carries an invalid sequence")
		}
		to, err := sequenceFromDB(hits[i].ToSequence)
		if err != nil {
			return nil, Errorf(ErrorCodeProjectionCorrupt, "projected document carries an invalid sequence")
		}
		documents = append(documents, RecallDocument{
			ID:            hits[i].ID,
			SessionID:     hits[i].SessionID,
			FromSequence:  from,
			ToSequence:    to,
			Content:       hits[i].Content,
			Trust:         approval.TrustLabel(hits[i].TrustLabel),
			PromptVersion: hits[i].PromptVersion,
			ModelProtocol: hits[i].ModelProtocol,
			ModelName:     hits[i].ModelName,
			Score:         hits[i].Rank,
			CreatedAt:     hits[i].CreatedAt,
			ExpiresAt:     hits[i].ExpiresAt,
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

func provenanceFor(documents []RecallDocument) []ProvenanceRef {
	refs := make([]ProvenanceRef, 0, len(documents))
	for i := range documents {
		document := &documents[i]
		refs = append(refs, ProvenanceRef{
			DocumentID:   document.ID,
			SessionID:    document.SessionID,
			FromSequence: document.FromSequence,
			ToSequence:   document.ToSequence,
		})
	}
	return refs
}

func (s *Service) checkSummaryCapability(query string, documents []RecallDocument, model *SummaryModel) (int, error) {
	if model == nil {
		return 0, Errorf(ErrorCodeInvalidArgument, "summary model must not be nil")
	}
	if strings.TrimSpace(model.Protocol) == "" || strings.TrimSpace(model.Name) == "" {
		return 0, Errorf(ErrorCodeInvalidArgument, "summary model identity must not be empty")
	}
	if !model.StructuredOutput {
		return 0, Errorf(ErrorCodeModelCapabilityUnsupported, "summary model lacks structured output")
	}
	input := estimateTokens(query)
	for i := range documents {
		input += estimateTokens(documents[i].Content)
	}
	if strings.TrimSpace(model.Tokenizer) == "" {
		input *= 2
	}
	if model.ContextTokens > 0 && input+s.tokens > model.ContextTokens {
		return 0, Errorf(ErrorCodeModelCapabilityUnsupported, "summary input exceeds model context")
	}
	return s.tokens, nil
}

func (s *Service) Summarize(ctx context.Context, ownerID, sessionID, query string, documents []RecallDocument, model *SummaryModel, summarizer Summarizer) (RecallContext, error) {
	if ctx == nil {
		return RecallContext{}, Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return RecallContext{}, err
	}
	if strings.TrimSpace(ownerID) == "" || strings.TrimSpace(sessionID) == "" {
		return RecallContext{}, Errorf(ErrorCodeInvalidArgument, "owner and session must not be empty")
	}
	if len(documents) == 0 {
		return RecallContext{}, Errorf(ErrorCodeInvalidArgument, "summary requires at least one document")
	}
	if summarizer == nil {
		return RecallContext{}, Errorf(ErrorCodeInvalidArgument, "summarizer must not be nil")
	}
	maxTokens, err := s.checkSummaryCapability(query, documents, model)
	if err != nil {
		return RecallContext{}, err
	}
	summary, err := summarizer.Summarize(ctx, &SummaryPrompt{
		Version:   s.summaryPrompt,
		Query:     query,
		Documents: documents,
		MaxTokens: maxTokens,
	})
	if err != nil {
		return RecallContext{}, err
	}
	content, ok := cleanText(summary, s.maxBytes)
	if !ok {
		return RecallContext{}, Errorf(ErrorCodeUnavailable, "summarizer returned no usable content")
	}
	if s.secrets != nil && s.secrets.Contains(content) {
		return RecallContext{}, Errorf(ErrorCodeUnavailable, "summarizer returned no usable content")
	}
	from, to := documents[0].FromSequence, documents[0].ToSequence
	for i := range documents[1:] {
		document := &documents[1+i]
		if document.FromSequence < from {
			from = document.FromSequence
		}
		if document.ToSequence > to {
			to = document.ToSequence
		}
	}
	now := time.Now().UTC()
	record := &StoredDocument{
		ID:            documentID(sessionID, KindSummary, from, to, s.summaryPrompt),
		OwnerID:       ownerID,
		SessionID:     sessionID,
		Kind:          KindSummary,
		Content:       content,
		TrustLabel:    string(approval.TrustDerivedUntrusted),
		PromptVersion: s.summaryPrompt,
		ModelProtocol: model.Protocol,
		ModelName:     model.Name,
		CreatedAt:     now,
	}
	if record.FromSequence, err = sequenceToDB(from); err != nil {
		return RecallContext{}, Errorf(ErrorCodeProjectionCorrupt, "summary source range is not valid")
	}
	if record.ToSequence, err = sequenceToDB(to); err != nil {
		return RecallContext{}, Errorf(ErrorCodeProjectionCorrupt, "summary source range is not valid")
	}
	if s.summaryTTL > 0 {
		expires := now.Add(s.summaryTTL)
		record.ExpiresAt = &expires
	}
	if _, err := s.documents.UpsertDocument(ctx, record); err != nil {
		return RecallContext{}, err
	}
	return RecallContext{
		Query:      query,
		Documents:  documents,
		Summary:    content,
		Provenance: provenanceFor(documents),
		Trust:      approval.TrustDerivedUntrusted,
	}, nil
}

func (s *Service) RecallWithSummary(ctx context.Context, req *RecallRequest, model *SummaryModel, summarizer Summarizer) (RecallContext, error) {
	documents, err := s.Recall(ctx, req)
	if err != nil {
		return RecallContext{}, err
	}
	if model == nil || summarizer == nil || len(documents) == 0 {
		return RecallContext{Query: req.Query, Documents: documents, Provenance: provenanceFor(documents)}, nil
	}
	return s.Summarize(ctx, req.OwnerID, req.SessionID, req.Query, documents, model, summarizer)
}
