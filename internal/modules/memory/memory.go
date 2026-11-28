package memory

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"github.com/anggasct/aura/internal/platform/fault"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/textsearch"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/platform/turn/ingress"
)

const (
	KindEventText = "event_text"
	KindSummary   = "summary"
)

const (
	defaultMaxDocuments      = 10
	defaultTokenBudget       = 2000
	hardMaxDocuments         = 50
	hardMaxTokenBudget       = 8000
	maxQueryRunes            = runtimeingress.MaxQueryRunes
	maxContentBytes          = 1 << 16
	maxSearchRowContentBytes = 2048
)

const (
	defaultSummaryPromptVersion = "memory-summary-v1"
	defaultSummaryTTL           = 720 * time.Hour
)

const (
	defaultFallbackMaxDocuments = 5
	defaultFallbackTokenBudget  = 1000
)

const (
	defaultRangeThreshold  = 20
	defaultRangeLiveWindow = 5
	maxRangeThreshold      = 200
	maxRangeLiveWindow     = 50
	maxRangeFetch          = 200
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
	Tainted      bool
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
	ListExpiredSummaries(ctx context.Context, now time.Time, limit int) ([]StoredDocument, error)
	ListDocuments(ctx context.Context, filter *DocumentFilter) ([]StoredDocument, error)
	ListSessionDocumentsByKind(ctx context.Context, ownerID, sessionID, kind string, limit int) ([]StoredDocument, error)
	ListSessionDocumentsByKindInRange(ctx context.Context, ownerID, sessionID, kind string, from, to int64, limit int) ([]StoredDocument, error)
	DeleteSessionSummaries(ctx context.Context, ownerID, sessionID string) (int, error)
	DeleteOtherSummaries(ctx context.Context, ownerID, sessionID, keepID string) (int, error)
	GetDocument(ctx context.Context, id string) (StoredDocument, bool, error)
	CountSessionDocuments(ctx context.Context, sessionID string) (int, error)
	DeleteDocumentsByIDsOwned(ctx context.Context, ownerID string, ids []string) (int, error)
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
	Tainted       *bool
	PromptVersion string
	ModelProtocol string
	ModelName     string
	AgentID       string
	CreatedAt     time.Time
	ExpiresAt     *time.Time
}

func (d *StoredDocument) IsTainted() bool {
	return d == nil || d.Tainted == nil || *d.Tainted
}

type DocumentFilter struct {
	OwnerID   string
	SessionID string
	AgentIDs  []string
	Limit     int
}

type MatchMode int

const (
	MatchStrict MatchMode = iota
	MatchFallbackOR
)

type StoredQuery struct {
	OwnerID   string
	AgentIDs  []string
	SessionID string
	Terms     string
	Mode      MatchMode
	After     time.Time
	Before    time.Time
	Now       time.Time
	Limit     int
}

type StoredHit struct {
	StoredDocument
	Rank  float64
	Score float64
}

type RecallRequest struct {
	OwnerID        string
	PrincipalClass principal.Class
	AgentIDs       []string
	SessionID      string
	Query          string
	MaxDocuments   int
	TokenBudget    int
	Before         time.Time
	Now            time.Time
}

type SearchRequest struct {
	OwnerID        string
	PrincipalClass principal.Class
	AgentIDs       []string
	SessionID      string
	Query          string
	After          time.Time
	Before         time.Time
	Now            time.Time
	Limit          int
}

type TaintMarksReader interface {
	SessionMarks(ctx context.Context, sessionIDs []string) (map[string]uint64, error)
}

type RecallDocument struct {
	ID            string
	SessionID     string
	FromSequence  uint64
	ToSequence    uint64
	Content       string
	Trust         approval.TrustLabel
	Tainted       bool
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
	Tainted    bool
	Mode       string
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

type ServiceOptions struct {
	MaxDocuments         int
	TokenBudget          int
	MaxContentBytes      int
	SummaryPromptVersion string
	SummaryTTL           time.Duration
	FallbackMaxDocuments int
	FallbackTokenBudget  int
	RangeThreshold       int
	RangeLiveWindow      int
	TaintMarks           TaintMarksReader
	Observer             Observer
	EmbeddingEnabled     bool
	Embedder             Embedder
	Embedding            *EmbeddingSettings
	SecretSource         SecretSource
	Vectors              VectorStore
	MaxVectorScanRows    int
}

type Service struct {
	documents          DocumentStore
	secrets            Secrets
	secretSource       SecretSource
	taintMarks         TaintMarksReader
	observer           Observer
	embeddingEnabled   bool
	embedder           Embedder
	embedding          *EmbeddingSettings
	vectors            VectorStore
	maxVectorScanRows  int
	maxDocs            int
	tokens             int
	maxBytes           int
	summaryPrompt      string
	summaryTTL         time.Duration
	fallbackDocs       int
	fallbackTokens     int
	rangeThreshold     int
	rangeLiveWindow    int
	secretDrops        atomic.Int64
	projectionFailures atomic.Int64
	projectMu          sync.Mutex
}

func NewService(documents DocumentStore, secrets Secrets, config *ServiceOptions) (*Service, error) {
	if documents == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "document store must not be nil")
	}
	if config == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "config must not be nil")
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
	fallbackDocs := config.FallbackMaxDocuments
	if fallbackDocs == 0 {
		fallbackDocs = defaultFallbackMaxDocuments
	}
	fallbackTokens := config.FallbackTokenBudget
	if fallbackTokens == 0 {
		fallbackTokens = defaultFallbackTokenBudget
	}
	if fallbackDocs <= 0 || fallbackDocs > hardMaxDocuments || fallbackTokens <= 0 || fallbackTokens > hardMaxTokenBudget {
		return nil, fault.New(ErrorCodeBudgetInvalid, "fallback budget is not within bounds")
	}
	rangeThreshold := config.RangeThreshold
	if rangeThreshold == 0 {
		rangeThreshold = defaultRangeThreshold
	}
	rangeLiveWindow := config.RangeLiveWindow
	if rangeLiveWindow == 0 {
		rangeLiveWindow = defaultRangeLiveWindow
	}
	if rangeThreshold <= 0 || rangeThreshold > maxRangeThreshold || rangeLiveWindow <= 0 || rangeLiveWindow > maxRangeLiveWindow {
		return nil, fault.New(ErrorCodeBudgetInvalid, "range budget is not within bounds")
	}
	vectors := config.Vectors
	if vectors == nil {
		if vs, ok := documents.(VectorStore); ok {
			vectors = vs
		}
	}
	maxScan := config.MaxVectorScanRows
	if maxScan <= 0 {
		maxScan = maxVectorScanRows
	}
	return &Service{
		documents:         documents,
		secrets:           secrets,
		secretSource:      config.SecretSource,
		taintMarks:        config.TaintMarks,
		observer:          config.Observer,
		embeddingEnabled:  config.EmbeddingEnabled,
		embedder:          config.Embedder,
		embedding:         config.Embedding,
		vectors:           vectors,
		maxVectorScanRows: maxScan,
		maxDocs:           maxDocs,
		tokens:            tokens,
		maxBytes:          maxBytes,
		summaryPrompt:     summaryPrompt,
		summaryTTL:        summaryTTL,
		fallbackDocs:      fallbackDocs,
		fallbackTokens:    fallbackTokens,
		rangeThreshold:    rangeThreshold,
		rangeLiveWindow:   rangeLiveWindow,
	}, nil
}

func (s *Service) SecretDropCount() int64 {
	if s == nil {
		return 0
	}
	return s.secretDrops.Load()
}

func (s *Service) ProjectionFailureCount() int64 {
	if s == nil {
		return 0
	}
	return s.projectionFailures.Load()
}

func (s *Service) RunPurgeLoop(ctx context.Context, interval time.Duration, limit int) {
	if s == nil || interval <= 0 || limit <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.PurgeExpired(ctx, time.Now().UTC(), limit); err != nil {
				s.observe(ctx, &Observation{Outcome: "purge_error"})
			}
		}
	}
}

func trustFor(ownerID, author, kind string) (approval.TrustLabel, bool) {
	switch kind {
	case "tool.completed":
		return approval.TrustUntrustedExternal, true
	case "message.completed":
		return approval.TrustDerivedUntrusted, true
	case "adk_event":
		switch author {
		case "user":
			if principal.ClassForPrincipal(ownerID) == principal.ClassOwner {
				return approval.TrustOwnerInput, true
			}
			return approval.TrustUntrustedExternal, true
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
		return 0, fault.New(ErrorCodeInvalidArgument, "sequence is not valid")
	}
	return int64(sequence), nil
}

func sequenceFromDB(sequence int64) (uint64, error) {
	if sequence <= 0 {
		return 0, fault.New(ErrorCodeInvalidArgument, "sequence is not valid")
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

func stripRecallEvidence(text string) (string, bool) {
	if !strings.Contains(text, turn.RecallEvidenceStart) {
		return text, false
	}
	had := false
	for {
		start := strings.Index(text, turn.RecallEvidenceStart)
		if start < 0 {
			break
		}
		had = true
		rest := text[start:]
		end := strings.Index(rest, turn.RecallEvidenceEnd)
		if end < 0 {
			return "", true
		}
		text = text[:start] + rest[end+len(turn.RecallEvidenceEnd):]
	}
	return text, had
}

func (s *Service) ProjectEvent(ctx context.Context, ownerID string, event *Event) (Document, bool, error) {
	var taintCtx *batchTaintContext
	if event != nil {
		taintCtx = s.buildBatchTaintContext(ctx, nil, []*Event{event})
	}
	doc, indexed, inserted, err := s.projectEvent(ctx, ownerID, event, taintCtx)
	if err != nil {
		return Document{}, false, err
	}
	if inserted {
		s.embedDocumentBestEffort(ctx, doc.ID, doc.OwnerID, doc.Content)
	}
	return doc, indexed, nil
}

func (s *Service) projectEvent(ctx context.Context, ownerID string, event *Event, taintCtx *batchTaintContext) (doc Document, indexed, inserted bool, err error) {
	if ctx == nil {
		return Document{}, false, false, fault.New(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return Document{}, false, false, err
	}
	if strings.TrimSpace(ownerID) == "" {
		return Document{}, false, false, fault.New(ErrorCodeInvalidArgument, "owner must not be empty")
	}
	if event == nil {
		return Document{}, false, false, fault.New(ErrorCodeInvalidArgument, "event must not be nil")
	}
	if event.SessionID == "" {
		return Document{}, false, false, fault.New(ErrorCodeInvalidArgument, "event session must not be empty")
	}
	if _, err := sequenceToDB(event.Sequence); err != nil {
		return Document{}, false, false, err
	}
	trust, indexable := trustFor(ownerID, event.Author, event.Kind)
	if !indexable {
		return Document{}, false, false, nil
	}
	text, ok := extractText(event.Payload)
	if !ok {
		return Document{}, false, false, nil
	}
	tainted := taintCtx.isEventTainted(ownerID, event)
	return s.writeProjection(ctx, ownerID, &projection{
		sessionID: event.SessionID, from: event.Sequence, to: event.Sequence, text: text, trust: trust, tainted: tainted, createdAt: event.CreatedAt,
	})
}

type projection struct {
	sessionID string
	from      uint64
	to        uint64
	text      string
	trust     approval.TrustLabel
	tainted   bool
	createdAt time.Time
}

func (s *Service) writeProjection(ctx context.Context, ownerID string, p *projection) (doc Document, indexed, inserted bool, err error) {
	from, err := sequenceToDB(p.from)
	if err != nil {
		return Document{}, false, false, err
	}
	to, err := sequenceToDB(p.to)
	if err != nil {
		return Document{}, false, false, err
	}
	text := p.text
	if stripped, had := stripRecallEvidence(text); had {
		if strings.TrimSpace(stripped) == "" {
			return Document{}, false, false, nil
		}
		text = stripped
	}
	content, ok := cleanText(text, s.maxBytes)
	if !ok {
		return Document{}, false, false, nil
	}
	if s.secrets != nil && s.secrets.Contains(content) {
		s.secretDrops.Add(1)
		return Document{}, false, false, nil
	}
	createdAt := p.createdAt.UTC()
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	document := Document{
		ID:           documentID(p.sessionID, KindEventText, p.from, p.to, ""),
		OwnerID:      ownerID,
		SessionID:    p.sessionID,
		Kind:         KindEventText,
		FromSequence: p.from,
		ToSequence:   p.to,
		Content:      content,
		Trust:        p.trust,
		Tainted:      p.tainted,
		CreatedAt:    createdAt,
	}
	inserted, err = s.documents.UpsertDocument(ctx, &StoredDocument{
		ID:           document.ID,
		OwnerID:      document.OwnerID,
		SessionID:    document.SessionID,
		Kind:         document.Kind,
		FromSequence: from,
		ToSequence:   to,
		Content:      document.Content,
		TrustLabel:   string(document.Trust),
		Tainted:      boolPtr(document.Tainted),
		CreatedAt:    document.CreatedAt,
	})
	if err != nil {
		return Document{}, false, false, err
	}
	if inserted && s.eventReopenedSummary(ctx, ownerID, p.sessionID, p.from) {
		if _, err := s.documents.DeleteSessionSummaries(ctx, ownerID, p.sessionID); err != nil {
			return Document{}, false, false, err
		}
	}
	return document, true, inserted, nil
}

func (s *Service) eventReopenedSummary(ctx context.Context, ownerID, sessionID string, sequence uint64) bool {
	summaries, err := s.documents.ListSessionDocumentsByKind(ctx, ownerID, sessionID, KindSummary, maxRangeSummaries)
	if err != nil {
		return false
	}
	for i := range summaries {
		from, err := sequenceFromDB(summaries[i].FromSequence)
		if err != nil {
			continue
		}
		to, err := sequenceFromDB(summaries[i].ToSequence)
		if err != nil {
			continue
		}
		if sequence >= from && sequence <= to {
			return true
		}
	}
	return false
}

func estimateTokens(content string) int {
	if tokens := len(content) / 4; tokens > 1 {
		return tokens
	}
	return 1
}

func (s *Service) Recall(ctx context.Context, req *RecallRequest) ([]RecallDocument, error) {
	documents, _, err := s.recallWithMode(ctx, req)
	return documents, err
}

func (s *Service) recallWithMode(ctx context.Context, req *RecallRequest) ([]RecallDocument, string, error) {
	if ctx == nil {
		return nil, ModeLexical, fault.New(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, ModeLexical, err
	}
	if req == nil {
		return nil, ModeLexical, fault.New(ErrorCodeInvalidArgument, "recall request must not be nil")
	}
	if strings.TrimSpace(req.OwnerID) == "" || strings.TrimSpace(req.SessionID) == "" {
		return nil, ModeLexical, fault.New(ErrorCodeQueryInvalid, "recall owner and session must not be empty")
	}
	if len(req.AgentIDs) == 0 || slices.Contains(req.AgentIDs, "") {
		return nil, ModeLexical, fault.New(ErrorCodeQueryInvalid, "recall readable personas must not be empty")
	}
	if len([]rune(req.Query)) == 0 || len([]rune(req.Query)) > maxQueryRunes {
		return nil, ModeLexical, fault.New(ErrorCodeQueryInvalid, "recall query is not within bounds")
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
		return nil, ModeLexical, fault.New(ErrorCodeBudgetInvalid, "recall budget is not within bounds")
	}
	queryVec, mode := s.prepareQueryVector(ctx, req.OwnerID, req.PrincipalClass, req.AgentIDs, "", time.Time{}, req.Before, req.Now, req.Query)
	hits, mode, err := s.searchHybrid(ctx, &StoredQuery{
		OwnerID:   req.OwnerID,
		AgentIDs:  req.AgentIDs,
		SessionID: req.SessionID,
		Terms:     req.Query,
		Mode:      MatchFallbackOR,
		Before:    req.Before,
		Now:       req.Now,
		Limit:     maxDocs,
	}, queryVec, mode)
	if err != nil {
		return nil, mode, err
	}
	primary := make([]RecallDocument, 0, len(hits))
	for i := range hits {
		document, err := recallDocumentFromHit(&hits[i])
		if err != nil {
			return nil, mode, err
		}
		primary = append(primary, document)
	}
	primary = s.orderPrimary(ctx, req.SessionID, primary)
	documents := make([]RecallDocument, 0, len(primary))
	used := 0
	for i := range primary {
		estimate := estimateTokens(primary[i].Content)
		if used+estimate > tokens {
			break
		}
		used += estimate
		documents = append(documents, primary[i])
	}
	if len(documents) < maxDocs && s.fallbackDocs > 0 && s.fallbackDocs <= hardMaxDocuments && s.fallbackTokens > 0 && s.fallbackTokens <= hardMaxTokenBudget {
		extra, fallbackMode := s.recallFallback(ctx, req, maxDocs, documents, queryVec, mode)
		documents = append(documents, extra...)
		if fallbackMode == ModeLexicalFallback {
			mode = ModeLexicalFallback
		}
	}
	return documents, mode, nil
}

func (s *Service) orderPrimary(ctx context.Context, sessionID string, primary []RecallDocument) []RecallDocument {
	if len(primary) < 2 || s == nil || s.rangeLiveWindow <= 0 {
		return primary
	}
	mark, found, err := s.documents.ProjectionWatermark(ctx, sessionID)
	if err != nil || !found {
		return primary
	}
	cutoff := mark - int64(s.rangeLiveWindow)
	live := make([]RecallDocument, 0, len(primary))
	summaries := make([]RecallDocument, 0, len(primary))
	old := make([]RecallDocument, 0, len(primary))
	for i := range primary {
		if primary[i].PromptVersion != "" {
			summaries = append(summaries, primary[i])
			continue
		}
		from, err := sequenceToDB(primary[i].FromSequence)
		if err != nil {
			old = append(old, primary[i])
			continue
		}
		if from > cutoff {
			live = append(live, primary[i])
			continue
		}
		old = append(old, primary[i])
	}
	if len(summaries) == 0 {
		return primary
	}
	ordered := make([]RecallDocument, 0, len(primary))
	ordered = append(ordered, live...)
	ordered = append(ordered, summaries...)
	ordered = append(ordered, old...)
	return ordered
}

func recallDocumentFromHit(hit *StoredHit) (RecallDocument, error) {
	from, err := sequenceFromDB(hit.FromSequence)
	if err != nil {
		return RecallDocument{}, fault.New(ErrorCodeProjectionCorrupt, "projected document carries an invalid sequence")
	}
	to, err := sequenceFromDB(hit.ToSequence)
	if err != nil {
		return RecallDocument{}, fault.New(ErrorCodeProjectionCorrupt, "projected document carries an invalid sequence")
	}
	score := hit.Rank
	if hit.Score > 0 {
		score = hit.Score
	}
	return RecallDocument{
		ID:            hit.ID,
		SessionID:     hit.SessionID,
		FromSequence:  from,
		ToSequence:    to,
		Content:       hit.Content,
		Trust:         approval.TrustLabel(hit.TrustLabel),
		Tainted:       hit.IsTainted(),
		PromptVersion: hit.PromptVersion,
		ModelProtocol: hit.ModelProtocol,
		ModelName:     hit.ModelName,
		Score:         score,
		CreatedAt:     hit.CreatedAt,
		ExpiresAt:     hit.ExpiresAt,
	}, nil
}

func truncateUTF8Bytes(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	const ellipsis = "…"
	if maxBytes <= len(ellipsis) {
		return ""
	}
	budget := maxBytes - len(ellipsis)
	cut := 0
	for i := range text {
		if i > budget {
			break
		}
		cut = i
	}
	return text[:cut] + ellipsis
}

func (s *Service) Search(ctx context.Context, req *SearchRequest) ([]StoredHit, error) {
	hits, _, err := s.SearchWithMode(ctx, req)
	return hits, err
}

func (s *Service) SearchWithMode(ctx context.Context, req *SearchRequest) ([]StoredHit, string, error) {
	if ctx == nil {
		return nil, ModeLexical, fault.New(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, ModeLexical, err
	}
	if req == nil {
		return nil, ModeLexical, fault.New(ErrorCodeInvalidArgument, "search request must not be nil")
	}
	if strings.TrimSpace(req.OwnerID) == "" {
		return nil, ModeLexical, fault.New(ErrorCodeQueryInvalid, "search owner must not be empty")
	}
	if slices.Contains(req.AgentIDs, "") {
		return nil, ModeLexical, fault.New(ErrorCodeQueryInvalid, "search readable personas must not contain empty ids")
	}
	if utf8.RuneCountInString(req.Query) == 0 || utf8.RuneCountInString(req.Query) > maxQueryRunes {
		return nil, ModeLexical, fault.New(ErrorCodeQueryInvalid, "search query is not within bounds")
	}
	if len(textsearch.Tokenize(req.Query)) == 0 {
		return nil, ModeLexical, fault.New(ErrorCodeQueryInvalid, "search query carries no searchable terms")
	}
	if !req.After.IsZero() && !req.Before.IsZero() && req.After.After(req.Before) {
		return nil, ModeLexical, fault.New(ErrorCodeQueryInvalid, "search after timestamp must not be after before timestamp")
	}
	limit := req.Limit
	if limit <= 0 || limit > maxSearchLimit {
		return nil, ModeLexical, fault.New(ErrorCodeInvalidArgument, "search limit is out of bounds")
	}
	fetchLimit := limit
	if s.secrets != nil {
		fetchLimit = min(limit*2, maxSearchLimit*2)
	}
	sessionID := strings.TrimSpace(req.SessionID)
	queryVec, mode := s.prepareQueryVector(ctx, req.OwnerID, req.PrincipalClass, req.AgentIDs, sessionID, req.After, req.Before, req.Now, req.Query)
	hits, mode, err := s.searchHybrid(ctx, &StoredQuery{
		OwnerID:   req.OwnerID,
		AgentIDs:  req.AgentIDs,
		SessionID: sessionID,
		Terms:     req.Query,
		Mode:      MatchFallbackOR,
		After:     req.After,
		Before:    req.Before,
		Now:       req.Now,
		Limit:     fetchLimit,
	}, queryVec, mode)
	if err != nil {
		return nil, mode, err
	}
	budget := s.tokens
	used := 0
	results := make([]StoredHit, 0, min(len(hits), limit))
	for i := range hits {
		if len(results) >= limit {
			break
		}
		if s.secrets != nil && s.secrets.Contains(hits[i].Content) {
			continue
		}
		hit := hits[i]
		hit.Content = truncateUTF8Bytes(hit.Content, maxSearchRowContentBytes)
		estimate := estimateTokens(hit.Content)
		if len(results) > 0 && budget > 0 && used+estimate > budget {
			break
		}
		used += estimate
		results = append(results, hit)
	}
	return results, mode, nil
}

func (s *Service) recallFallback(ctx context.Context, req *RecallRequest, maxDocs int, primary []RecallDocument, queryVec []float32, mode string) (extra []RecallDocument, fallbackMode string) {
	seen := make(map[string]struct{}, len(primary))
	for i := range primary {
		seen[primary[i].ID] = struct{}{}
	}
	var hits []StoredHit
	var err error
	hits, fallbackMode, err = s.searchHybrid(ctx, &StoredQuery{
		OwnerID:  req.OwnerID,
		AgentIDs: req.AgentIDs,
		Terms:    req.Query,
		Mode:     MatchFallbackOR,
		Before:   req.Before,
		Now:      req.Now,
		Limit:    maxDocs + s.fallbackDocs,
	}, queryVec, mode)
	if err != nil {
		return nil, fallbackMode
	}
	extra = make([]RecallDocument, 0, s.fallbackDocs)
	used := 0
	for i := range hits {
		if len(primary)+len(extra) >= hardMaxDocuments {
			break
		}
		if hits[i].SessionID == req.SessionID {
			continue
		}
		if _, ok := seen[hits[i].ID]; ok {
			continue
		}
		estimate := estimateTokens(hits[i].Content)
		if used+estimate > s.fallbackTokens {
			break
		}
		document, err := recallDocumentFromHit(&hits[i])
		if err != nil {
			continue
		}
		used += estimate
		seen[document.ID] = struct{}{}
		extra = append(extra, document)
		if len(extra) >= s.fallbackDocs {
			break
		}
	}
	return extra, fallbackMode
}

func (s *Service) Watermark(ctx context.Context, sessionID string) (mark uint64, found bool, err error) {
	if ctx == nil {
		return 0, false, fault.New(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if strings.TrimSpace(sessionID) == "" {
		return 0, false, fault.New(ErrorCodeInvalidArgument, "session id must not be empty")
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
		return 0, fault.New(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if strings.TrimSpace(ownerID) == "" || strings.TrimSpace(sessionID) == "" {
		return 0, fault.New(ErrorCodeInvalidArgument, "owner and session must not be empty")
	}
	if err := s.documents.DeleteSessionDocuments(ctx, sessionID); err != nil {
		return 0, err
	}
	taintCtx := s.buildBatchTaintContext(ctx, nil, events)
	count := 0
	var insertedDocs []Document
	for _, event := range events {
		if err := ctx.Err(); err != nil {
			return count, err
		}
		doc, indexed, inserted, err := s.projectEvent(ctx, ownerID, event, taintCtx)
		if err != nil {
			return count, err
		}
		if indexed {
			count++
		}
		if inserted {
			insertedDocs = append(insertedDocs, doc)
		}
	}
	for i := range insertedDocs {
		s.embedDocumentBestEffort(ctx, insertedDocs[i].ID, insertedDocs[i].OwnerID, insertedDocs[i].Content)
	}
	return count, nil
}

func (s *Service) PurgeExpired(ctx context.Context, now time.Time, limit int) (int, error) {
	if ctx == nil {
		return 0, fault.New(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if limit <= 0 || limit > maxPurgeLimit {
		return 0, fault.New(ErrorCodeInvalidArgument, "purge limit is out of bounds")
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
		return 0, fault.New(ErrorCodeInvalidArgument, "summary model must not be nil")
	}
	if strings.TrimSpace(model.Protocol) == "" || strings.TrimSpace(model.Name) == "" {
		return 0, fault.New(ErrorCodeInvalidArgument, "summary model identity must not be empty")
	}
	if !model.StructuredOutput {
		return 0, fault.New(ErrorCodeModelCapabilityUnsupported, "summary model lacks structured output")
	}
	input := estimateTokens(query)
	for i := range documents {
		input += estimateTokens(documents[i].Content)
	}
	if strings.TrimSpace(model.Tokenizer) == "" {
		input *= 2
	}
	if model.ContextTokens > 0 && input+s.tokens > model.ContextTokens {
		return 0, fault.New(ErrorCodeModelCapabilityUnsupported, "summary input exceeds model context")
	}
	return s.tokens, nil
}

func (s *Service) Summarize(ctx context.Context, ownerID, sessionID, query string, documents []RecallDocument, model *SummaryModel, summarizer Summarizer) (RecallContext, error) {
	if ctx == nil {
		return RecallContext{}, fault.New(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return RecallContext{}, err
	}
	if strings.TrimSpace(ownerID) == "" || strings.TrimSpace(sessionID) == "" {
		return RecallContext{}, fault.New(ErrorCodeInvalidArgument, "owner and session must not be empty")
	}
	if len(documents) == 0 {
		return RecallContext{}, fault.New(ErrorCodeInvalidArgument, "summary requires at least one document")
	}
	if summarizer == nil {
		return RecallContext{}, fault.New(ErrorCodeInvalidArgument, "summarizer must not be nil")
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
		return RecallContext{}, fault.New(ErrorCodeUnavailable, "summarizer returned no usable content")
	}
	if s.secrets != nil && s.secrets.Contains(content) {
		return RecallContext{}, fault.New(ErrorCodeUnavailable, "summarizer returned no usable content")
	}
	summaryTainted := false
	from, to := documents[0].FromSequence, documents[0].ToSequence
	if documents[0].Tainted {
		summaryTainted = true
	}
	for i := range documents[1:] {
		document := &documents[1+i]
		if document.Tainted {
			summaryTainted = true
		}
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
		Tainted:       boolPtr(summaryTainted),
		PromptVersion: s.summaryPrompt,
		ModelProtocol: model.Protocol,
		ModelName:     model.Name,
		CreatedAt:     now,
	}
	if record.FromSequence, err = sequenceToDB(from); err != nil {
		return RecallContext{}, fault.New(ErrorCodeProjectionCorrupt, "summary source range is not valid")
	}
	if record.ToSequence, err = sequenceToDB(to); err != nil {
		return RecallContext{}, fault.New(ErrorCodeProjectionCorrupt, "summary source range is not valid")
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
		Tainted:    summaryTainted,
	}, nil
}

func (s *Service) RecallWithSummary(ctx context.Context, req *RecallRequest, model *SummaryModel, summarizer Summarizer) (RecallContext, error) {
	documents, mode, err := s.recallWithMode(ctx, req)
	if err != nil {
		return RecallContext{}, err
	}
	if model == nil || summarizer == nil || len(documents) == 0 {
		anyTainted := false
		for i := range documents {
			if documents[i].Tainted {
				anyTainted = true
				break
			}
		}
		return RecallContext{Query: req.Query, Documents: documents, Provenance: provenanceFor(documents), Tainted: anyTainted, Mode: mode}, nil
	}
	res, err := s.Summarize(ctx, req.OwnerID, req.SessionID, req.Query, documents, model, summarizer)
	if err != nil {
		return RecallContext{}, err
	}
	res.Mode = mode
	return res, nil
}

func (c *RecallContext) Evidence() *turn.UntrustedRecall {
	documents := make([]turn.UntrustedDocument, 0, len(c.Documents))
	for i := range c.Documents {
		documents = append(documents, turn.UntrustedDocument{
			ID:           c.Documents[i].ID,
			SessionID:    c.Documents[i].SessionID,
			FromSequence: c.Documents[i].FromSequence,
			ToSequence:   c.Documents[i].ToSequence,
			Trust:        c.Documents[i].Trust,
			Tainted:      c.Documents[i].Tainted,
			Content:      c.Documents[i].Content,
		})
	}
	return &turn.UntrustedRecall{
		Query:     c.Query,
		Summary:   c.Summary,
		Trust:     c.Trust,
		Tainted:   c.Tainted,
		Documents: documents,
	}
}
