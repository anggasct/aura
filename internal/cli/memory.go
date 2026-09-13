package cli

import (
	"context"
	"database/sql"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/memory"
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

func buildMemoryProvider(cfg *config.Config, db *sql.DB, observer memory.Observer) (*memory.Provider, error) {
	service, err := memory.NewService(&memoryDocumentStore{store: store.NewMemoryStore(db)}, nil, memory.Config{
		MaxDocuments:         cfg.Memory.MaxDocuments,
		TokenBudget:          cfg.Memory.RecallTokenBudget,
		SummaryPromptVersion: cfg.Memory.SummaryPromptVersion,
		SummaryTTL:           time.Duration(cfg.Memory.SummaryTTL),
		Observer:             observer,
	})
	if err != nil {
		return nil, err
	}
	return memory.NewProvider(service, memory.ProviderConfig{})
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
