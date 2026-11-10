package cli

import (
	"context"
	"os"
	"strings"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/platform/egress"
	"github.com/anggasct/aura/internal/platform/embedding"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/store"
)

type knowledgeEmbedder struct {
	client *embedding.Client
}

func (e *knowledgeEmbedder) Embed(ctx context.Context, role knowledge.EmbedRole, texts []string) ([][]float32, error) {
	return e.client.Embed(ctx, embedding.Role(role), texts)
}

type knowledgeVectors struct {
	store store.KBEmbeddingStore
}

func (v *knowledgeVectors) EmbeddingState(ctx context.Context, collection, fingerprint string) (knowledge.EmbeddingState, error) {
	state, err := v.store.EmbeddingState(ctx, collection, fingerprint)
	if err != nil {
		return knowledge.EmbeddingState{}, translateKnowledgeStoreError(err)
	}
	return knowledge.EmbeddingState{
		Exists: state.Exists, Fingerprint: state.Fingerprint, Chunks: state.Chunks,
		Covered: state.Covered, PendingCharacters: state.PendingCharacters,
	}, nil
}

func (v *knowledgeVectors) StartEmbedding(ctx context.Context, collection, fingerprint string) error {
	return translateKnowledgeStoreError(v.store.StartEmbedding(ctx, collection, fingerprint))
}

func (v *knowledgeVectors) PendingChunks(ctx context.Context, collection, fingerprint string, afterRowID int64, limit int) ([]knowledge.PendingChunk, error) {
	chunks, err := v.store.PendingChunks(ctx, collection, fingerprint, afterRowID, limit)
	if err != nil {
		return nil, translateKnowledgeStoreError(err)
	}
	out := make([]knowledge.PendingChunk, 0, len(chunks))
	for i := range chunks {
		out = append(out, knowledge.PendingChunk{RowID: chunks[i].RowID, Heading: chunks[i].Heading, Content: chunks[i].Content, SHA256: chunks[i].SHA256})
	}
	return out, nil
}

func (v *knowledgeVectors) CommitVectors(ctx context.Context, collection, fingerprint string, vectors []knowledge.VectorWrite) (knowledge.CommitOutcome, error) {
	writes := make([]store.KBVectorWrite, 0, len(vectors))
	for i := range vectors {
		writes = append(writes, store.KBVectorWrite{RowID: vectors[i].RowID, ContentSHA256: vectors[i].ContentSHA256, Vector: vectors[i].Vector})
	}
	result, err := v.store.CommitVectors(ctx, collection, fingerprint, writes)
	if err != nil {
		return knowledge.CommitOutcome{}, translateKnowledgeStoreError(err)
	}
	return knowledge.CommitOutcome{Written: result.Written, Completed: result.Completed}, nil
}

func (v *knowledgeVectors) DisableEmbedding(ctx context.Context, collection string, apply bool) (knowledge.DisableOutcome, error) {
	result, err := v.store.DisableEmbedding(ctx, collection, apply)
	return knowledge.DisableOutcome{
		OptedIn: result.OptedIn, Vectors: result.Vectors, Applied: result.Applied, WALTruncated: result.WALTruncated,
	}, translateKnowledgeStoreError(err)
}

func configuredEmbedding(cfg *config.Config) *config.KnowledgeEmbedding {
	if cfg == nil || cfg.Knowledge == nil {
		return nil
	}
	return cfg.Knowledge.Embedding
}

func resolveEmbeddingCredential(ref string) (string, bool) {
	if ref == "" {
		return "", true
	}
	value, err := parseSecretRef(ref).Resolve()
	value = strings.TrimSpace(value)
	if err != nil || value == "" {
		return "", false
	}
	return value, true
}

type knowledgeEmbeddingWiring struct {
	settings *knowledge.EmbeddingSettings
	embedder knowledge.Embedder
}

func newKnowledgeEmbedding(cfg *config.Config, resolver egress.Resolver) (knowledgeEmbeddingWiring, error) {
	embeddingConfig := configuredEmbedding(cfg)
	if embeddingConfig == nil {
		return knowledgeEmbeddingWiring{}, nil
	}
	fingerprint, err := embedding.Fingerprint(embeddingConfig)
	if err != nil {
		return knowledgeEmbeddingWiring{}, err
	}
	endpoint, err := embeddingConfig.Endpoint()
	if err != nil {
		return knowledgeEmbeddingWiring{}, err
	}
	wiring := knowledgeEmbeddingWiring{settings: &knowledge.EmbeddingSettings{
		Fingerprint: fingerprint, Destination: endpoint.Authority(), Loopback: endpoint.Loopback,
		Model: embeddingConfig.Model, Dimensions: embeddingConfig.Dimensions, BatchSize: embeddingConfig.BatchSize,
		MaxMemory: int64(embeddingConfig.MaxMemory), PublicQueries: embeddingConfig.PublicQueries,
	}}
	credential, resolved := resolveEmbeddingCredential(embeddingConfig.CredentialRef)
	if !resolved {
		return wiring, nil
	}
	client, err := embedding.New(embeddingConfig, credential, resolver)
	if err != nil {
		return knowledgeEmbeddingWiring{}, err
	}
	wiring.embedder = &knowledgeEmbedder{client: client}
	return wiring, nil
}

func knowledgeEmbeddingReader(cfg *config.Config, dbPath string) func(ctx context.Context) (health.KnowledgeEmbeddingState, error) {
	return func(ctx context.Context) (health.KnowledgeEmbeddingState, error) {
		var state health.KnowledgeEmbeddingState
		fingerprint := ""
		dimensions := 0
		if embeddingConfig := configuredEmbedding(cfg); embeddingConfig != nil {
			computed, err := embedding.Fingerprint(embeddingConfig)
			if err != nil {
				return health.KnowledgeEmbeddingState{}, err
			}
			fingerprint = computed
			_, state.Available = resolveEmbeddingCredential(embeddingConfig.CredentialRef)
			state.Configured = true
			state.MaxMemory = int64(embeddingConfig.MaxMemory)
			dimensions = embeddingConfig.Dimensions
		}
		if _, err := os.Stat(dbPath); err != nil {
			return health.KnowledgeEmbeddingState{}, err
		}
		db, err := store.OpenReadOnly(ctx, dbPath)
		if err != nil {
			return health.KnowledgeEmbeddingState{}, err
		}
		defer func() { _ = db.Close() }()
		present, err := store.KBEmbeddingPresent(ctx, db)
		if err != nil || !present {
			return state, err
		}
		coverage, err := store.NewKBEmbeddingStore(db).EmbeddingCoverage(ctx, fingerprint)
		if err != nil {
			return health.KnowledgeEmbeddingState{}, err
		}
		for i := range coverage {
			state.Collections = append(state.Collections, health.KnowledgeEmbeddingCollection{
				Collection: coverage[i].Collection, Chunks: coverage[i].Chunks, Covered: coverage[i].Covered,
				Stale: state.Configured && coverage[i].Fingerprint != fingerprint,
				Bytes: knowledge.VectorBytes(coverage[i].Chunks, dimensions),
			})
		}
		return state, nil
	}
}
