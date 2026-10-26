package cli

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/memory"
	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/secret"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/telemetry"
)

type memoryDocumentStore struct {
	store store.MemoryStore
}

func (s *memoryDocumentStore) UpsertDocument(ctx context.Context, document *memory.StoredDocument) (bool, error) {
	if document == nil {
		return false, memory.Errorf(memory.ErrorCodeInvalidArgument, "memory document must not be nil")
	}
	record := &store.MemoryDocument{
		ID:            document.ID,
		OwnerID:       document.OwnerID,
		SessionID:     document.SessionID,
		Kind:          document.Kind,
		FromSequence:  document.FromSequence,
		ToSequence:    document.ToSequence,
		Content:       document.Content,
		TrustLabel:    document.TrustLabel,
		PromptVersion: document.PromptVersion,
		ModelProtocol: document.ModelProtocol,
		ModelName:     document.ModelName,
		CreatedAt:     document.CreatedAt,
		ExpiresAt:     document.ExpiresAt,
	}
	return s.store.UpsertDocument(ctx, record)
}

func (s *memoryDocumentStore) ProjectionWatermark(ctx context.Context, sessionID string) (mark int64, found bool, err error) {
	return s.store.ProjectionWatermark(ctx, sessionID)
}

func (s *memoryDocumentStore) Search(ctx context.Context, query *memory.StoredQuery) ([]memory.StoredHit, error) {
	hits, err := s.store.Search(ctx, &store.MemoryQuery{
		OwnerID:   query.OwnerID,
		SessionID: query.SessionID,
		Terms:     query.Terms,
		Before:    query.Before,
		Limit:     query.Limit,
	})
	if err != nil {
		return nil, err
	}
	converted := make([]memory.StoredHit, 0, len(hits))
	for i := range hits {
		converted = append(converted, memory.StoredHit{
			StoredDocument: memory.StoredDocument{
				ID:            hits[i].ID,
				OwnerID:       hits[i].OwnerID,
				SessionID:     hits[i].SessionID,
				Kind:          hits[i].Kind,
				FromSequence:  hits[i].FromSequence,
				ToSequence:    hits[i].ToSequence,
				Content:       hits[i].Content,
				TrustLabel:    hits[i].TrustLabel,
				PromptVersion: hits[i].PromptVersion,
				ModelProtocol: hits[i].ModelProtocol,
				ModelName:     hits[i].ModelName,
				CreatedAt:     hits[i].CreatedAt,
				ExpiresAt:     hits[i].ExpiresAt,
			},
			Rank: hits[i].Rank,
		})
	}
	return converted, nil
}

func (s *memoryDocumentStore) DeleteSessionDocuments(ctx context.Context, sessionID string) error {
	return s.store.DeleteSessionDocuments(ctx, sessionID)
}

func (s *memoryDocumentStore) DeleteExpiredSummaries(ctx context.Context, now time.Time, limit int) (int, error) {
	return s.store.DeleteExpiredSummaries(ctx, now, limit)
}

func (s *memoryDocumentStore) GetDocument(ctx context.Context, id string) (memory.StoredDocument, bool, error) {
	document, found, err := s.store.GetDocument(ctx, id)
	if err != nil || !found {
		return memory.StoredDocument{}, found, err
	}
	return memory.StoredDocument{
		ID:            document.ID,
		OwnerID:       document.OwnerID,
		SessionID:     document.SessionID,
		Kind:          document.Kind,
		FromSequence:  document.FromSequence,
		ToSequence:    document.ToSequence,
		Content:       document.Content,
		TrustLabel:    document.TrustLabel,
		PromptVersion: document.PromptVersion,
		ModelProtocol: document.ModelProtocol,
		ModelName:     document.ModelName,
		CreatedAt:     document.CreatedAt,
		ExpiresAt:     document.ExpiresAt,
	}, true, nil
}

func (s *memoryDocumentStore) ListSessionDocumentsByKind(ctx context.Context, ownerID, sessionID, kind string, limit int) ([]memory.StoredDocument, error) {
	documents, err := s.store.ListSessionDocumentsByKind(ctx, ownerID, sessionID, kind, limit)
	if err != nil {
		return nil, err
	}
	return convertMemoryDocuments(documents), nil
}

func (s *memoryDocumentStore) ListSessionDocumentsByKindInRange(ctx context.Context, ownerID, sessionID, kind string, from, to int64, limit int) ([]memory.StoredDocument, error) {
	documents, err := s.store.ListSessionDocumentsByKindInRange(ctx, ownerID, sessionID, kind, from, to, limit)
	if err != nil {
		return nil, err
	}
	return convertMemoryDocuments(documents), nil
}

func convertMemoryDocuments(documents []store.MemoryDocument) []memory.StoredDocument {
	converted := make([]memory.StoredDocument, 0, len(documents))
	for i := range documents {
		converted = append(converted, memory.StoredDocument{
			ID:            documents[i].ID,
			OwnerID:       documents[i].OwnerID,
			SessionID:     documents[i].SessionID,
			Kind:          documents[i].Kind,
			FromSequence:  documents[i].FromSequence,
			ToSequence:    documents[i].ToSequence,
			Content:       documents[i].Content,
			TrustLabel:    documents[i].TrustLabel,
			PromptVersion: documents[i].PromptVersion,
			ModelProtocol: documents[i].ModelProtocol,
			ModelName:     documents[i].ModelName,
			CreatedAt:     documents[i].CreatedAt,
			ExpiresAt:     documents[i].ExpiresAt,
		})
	}
	return converted
}

func (s *memoryDocumentStore) ListSessionDocuments(ctx context.Context, ownerID, sessionID string, limit int) ([]memory.StoredDocument, error) {
	documents, err := s.store.ListSessionDocuments(ctx, ownerID, sessionID, limit)
	if err != nil {
		return nil, err
	}
	converted := make([]memory.StoredDocument, 0, len(documents))
	for i := range documents {
		converted = append(converted, memory.StoredDocument{
			ID:            documents[i].ID,
			OwnerID:       documents[i].OwnerID,
			SessionID:     documents[i].SessionID,
			Kind:          documents[i].Kind,
			FromSequence:  documents[i].FromSequence,
			ToSequence:    documents[i].ToSequence,
			Content:       documents[i].Content,
			TrustLabel:    documents[i].TrustLabel,
			PromptVersion: documents[i].PromptVersion,
			ModelProtocol: documents[i].ModelProtocol,
			ModelName:     documents[i].ModelName,
			CreatedAt:     documents[i].CreatedAt,
			ExpiresAt:     documents[i].ExpiresAt,
		})
	}
	return converted, nil
}

func (s *memoryDocumentStore) ListOwnerDocuments(ctx context.Context, ownerID string, limit int) ([]memory.StoredDocument, error) {
	documents, err := s.store.ListOwnerDocuments(ctx, ownerID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]memory.StoredDocument, 0, len(documents))
	for i := range documents {
		out = append(out, memory.StoredDocument{
			ID:            documents[i].ID,
			OwnerID:       documents[i].OwnerID,
			SessionID:     documents[i].SessionID,
			Kind:          documents[i].Kind,
			FromSequence:  documents[i].FromSequence,
			ToSequence:    documents[i].ToSequence,
			Content:       documents[i].Content,
			TrustLabel:    documents[i].TrustLabel,
			PromptVersion: documents[i].PromptVersion,
			ModelProtocol: documents[i].ModelProtocol,
			ModelName:     documents[i].ModelName,
			CreatedAt:     documents[i].CreatedAt,
			ExpiresAt:     documents[i].ExpiresAt,
		})
	}
	return out, nil
}

func (s *memoryDocumentStore) CountSessionDocuments(ctx context.Context, sessionID string) (int, error) {
	return s.store.CountSessionDocuments(ctx, sessionID)
}

func (s *memoryDocumentStore) DeleteDocumentsByIDsOwned(ctx context.Context, ownerID string, ids []string) (int, error) {
	return s.store.DeleteDocumentsByIDsOwned(ctx, ownerID, ids)
}

func (s *memoryDocumentStore) DeleteSessionSummaries(ctx context.Context, ownerID, sessionID string) (int, error) {
	return s.store.DeleteSessionSummaries(ctx, ownerID, sessionID)
}

func (s *memoryDocumentStore) DeleteOtherSummaries(ctx context.Context, ownerID, sessionID, keepID string) (int, error) {
	return s.store.DeleteOtherSummaries(ctx, ownerID, sessionID, keepID)
}

func buildMemoryService(cfg *config.Config, db *sql.DB, observer memory.Observer) (*memory.Service, error) {
	return memory.NewService(&memoryDocumentStore{store: store.NewMemoryStore(db)}, secret.New(), &memory.Config{
		MaxDocuments:         cfg.Memory.MaxDocuments,
		TokenBudget:          cfg.Memory.RecallTokenBudget,
		SummaryPromptVersion: cfg.Memory.SummaryPromptVersion,
		SummaryTTL:           time.Duration(cfg.Memory.SummaryTTL),
		RangeThreshold:       cfg.Memory.RangeThreshold,
		RangeLiveWindow:      cfg.Memory.RangeLiveWindow,
		Observer:             observer,
	})
}

const (
	memorySummaryOutputBound = 1 << 16
	memorySummaryMaxTokens   = 1 << 14
)

type memoryRuntime struct {
	provider   *memory.Provider
	service    *memory.Service
	model      *memory.SummaryModel
	summarizer memory.Summarizer
}

func buildMemoryRuntime(ctx context.Context, cfg *config.Config, db *sql.DB, logger *slog.Logger, observer memory.Observer) (*memoryRuntime, error) {
	service, err := buildMemoryService(cfg, db, observer)
	if err != nil {
		return nil, err
	}
	model, summarizer := buildMemorySummary(ctx, cfg, logger, observer)
	provider, err := memory.NewProvider(service, memory.ProviderConfig{})
	if err != nil {
		return nil, err
	}
	return &memoryRuntime{provider: provider, service: service, model: model, summarizer: summarizer}, nil
}

func buildMemorySummary(ctx context.Context, cfg *config.Config, logger *slog.Logger, observer memory.Observer) (*memory.SummaryModel, memory.Summarizer) {
	if cfg == nil {
		return nil, nil
	}
	role := strings.TrimSpace(cfg.Models.Routing["summarize"])
	if role == "" {
		role = "auxiliary"
	}
	candidate := role
	if route, ok := cfg.ModelRoutes[role]; ok && len(route.Candidates) > 0 {
		candidate = route.Candidates[0]
	}
	definition, ok := cfg.Models.Definitions[candidate]
	if !ok || strings.TrimSpace(definition.Model) == "" {
		observeUnavailableSummary(ctx, observer)
		return nil, nil
	}
	modelName := definition.Model
	if _, routed := cfg.ModelRoutes[role]; routed {
		modelName = role
	}
	llm, err := adkmodel.NewLLM(ctx, modelName)
	if err != nil {
		if logger != nil {
			logger.WarnContext(ctx, "memory range summarizer unavailable", "component", "memory", "route", role, "model", definition.Model, "error", err)
		}
		observeUnavailableSummary(ctx, observer)
		return nil, nil
	}
	model := &memory.SummaryModel{
		Protocol:         definition.Protocol,
		Name:             definition.Model,
		Tokenizer:        definition.Capabilities.Tokenizer,
		ContextTokens:    definition.Capabilities.ContextTokens,
		StructuredOutput: definition.Capabilities.StructuredOutput,
	}
	return model, &llmSummarizer{llm: llm, bound: memorySummaryOutputBound}
}

func observeUnavailableSummary(ctx context.Context, observer memory.Observer) {
	if observer == nil {
		return
	}
	observer(ctx, &memory.Observation{Outcome: string(memory.ErrorCodeUnavailable)})
}

type llmSummarizer struct {
	llm   adkmodel.LLM
	bound int
}

func (s *llmSummarizer) Summarize(ctx context.Context, prompt *memory.SummaryPrompt) (string, error) {
	if ctx == nil {
		return "", memory.Errorf(memory.ErrorCodeInvalidArgument, "context must not be nil")
	}
	if s == nil || s.llm == nil {
		return "", memory.Errorf(memory.ErrorCodeInvalidArgument, "memory summarizer has no model")
	}
	if prompt == nil {
		return "", memory.Errorf(memory.ErrorCodeInvalidArgument, "summary prompt must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	maxTokens := prompt.MaxTokens
	if maxTokens <= 0 || maxTokens > memorySummaryMaxTokens {
		maxTokens = memorySummaryMaxTokens
	}
	var request strings.Builder
	request.WriteString("Summarize the closed session range below into a compact factual digest. Do not invent details.\n\n")
	for i := range prompt.Documents {
		fmt.Fprintf(&request, "[seq %d-%d trust=%s]\n%s\n\n",
			prompt.Documents[i].FromSequence, prompt.Documents[i].ToSequence,
			prompt.Documents[i].Trust, prompt.Documents[i].Content)
	}
	var collected strings.Builder
	for response, err := range s.llm.GenerateContent(ctx, &adkmodel.LLMRequest{
		Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: request.String()}}}},
		Config:   &genai.GenerateContentConfig{MaxOutputTokens: int32(maxTokens)},
	}, false) {
		if err != nil {
			return "", err
		}
		if response == nil || response.Content == nil {
			continue
		}
		for _, part := range response.Content.Parts {
			if part == nil || strings.TrimSpace(part.Text) == "" {
				continue
			}
			if collected.Len()+len(part.Text) > s.bound {
				return collected.String(), nil
			}
			collected.WriteString(part.Text)
		}
	}
	if strings.TrimSpace(collected.String()) == "" {
		return "", memory.Errorf(memory.ErrorCodeUnavailable, "summarizer returned no usable content")
	}
	return collected.String(), nil
}

func buildMemoryHook(service *memory.Service, model *memory.SummaryModel, summarizer memory.Summarizer, logger *slog.Logger) func(ctx context.Context, req *runtime.TurnRequest, events []store.RuntimeEvent) error {
	return func(ctx context.Context, req *runtime.TurnRequest, events []store.RuntimeEvent) error {
		if service == nil {
			return memory.Errorf(memory.ErrorCodeInvalidArgument, "memory service must not be nil")
		}
		if req == nil {
			return memory.Errorf(memory.ErrorCodeInvalidArgument, "turn request must not be nil")
		}
		converted := make([]*memory.Event, 0, len(events))
		for i := range events {
			converted = append(converted, &memory.Event{
				ID:        events[i].ID,
				SessionID: events[i].SessionID,
				Sequence:  events[i].Sequence,
				TurnID:    events[i].TurnID,
				Author:    events[i].Author,
				Kind:      events[i].Kind,
				Payload:   events[i].Payload,
				CreatedAt: events[i].CreatedAt,
			})
		}
		result, err := service.ProjectBatch(ctx, req.PrincipalID, converted)
		if result.Dropped > 0 && logger != nil {
			logger.DebugContext(ctx, "memory projection dropped secrets", "dropped", result.Dropped, "turn_id", req.TurnID)
		}
		if _, purgeErr := service.PurgeExpired(ctx, time.Now().UTC(), 100); purgeErr != nil && logger != nil {
			logger.WarnContext(ctx, "memory expiry failed", "error", purgeErr, "turn_id", req.TurnID)
		}
		if model != nil && summarizer != nil {
			if _, _, summaryErr := service.SummarizeClosedRange(ctx, req.PrincipalID, req.SessionID, model, summarizer); summaryErr != nil && logger != nil {
				logger.WarnContext(ctx, "memory range summary failed", "error", summaryErr, "turn_id", req.TurnID)
			}
		}
		return err
	}
}

func memoryRecorderObserver(recorder *telemetry.MemoryRecorder) memory.Observer {
	if recorder == nil {
		return nil
	}
	return func(ctx context.Context, observation *memory.Observation) {
		recorder.Record(ctx, &telemetry.MemoryObservation{
			Outcome:     observation.Outcome,
			Summarized:  observation.Summarized,
			Documents:   observation.Documents,
			Screened:    observation.Screened,
			ModelSystem: observation.ModelProtocol,
			Duration:    observation.Duration,
		})
	}
}
