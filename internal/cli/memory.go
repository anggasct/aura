package cli

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

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

func (s *memoryDocumentStore) ListSessionDocuments(ctx context.Context, ownerID, sessionID string, limit int) ([]memory.StoredDocument, error) {
	documents, err := s.store.ListSessionDocuments(ctx, ownerID, sessionID, limit)
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

func buildMemoryService(cfg *config.Config, db *sql.DB, observer memory.Observer) (*memory.Service, error) {
	return memory.NewService(&memoryDocumentStore{store: store.NewMemoryStore(db)}, secret.New(), memory.Config{
		MaxDocuments:         cfg.Memory.MaxDocuments,
		TokenBudget:          cfg.Memory.RecallTokenBudget,
		SummaryPromptVersion: cfg.Memory.SummaryPromptVersion,
		SummaryTTL:           time.Duration(cfg.Memory.SummaryTTL),
		Observer:             observer,
	})
}

func buildMemoryProvider(cfg *config.Config, db *sql.DB, observer memory.Observer) (*memory.Provider, *memory.Service, error) {
	service, err := buildMemoryService(cfg, db, observer)
	if err != nil {
		return nil, nil, err
	}
	provider, err := memory.NewProvider(service, memory.ProviderConfig{})
	if err != nil {
		return nil, nil, err
	}
	return provider, service, nil
}

func buildMemoryHook(service *memory.Service, logger *slog.Logger) func(ctx context.Context, req *runtime.TurnRequest, events []store.RuntimeEvent) error {
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
