package app

import (
	"context"
	"os"
	"strings"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/platform/egress"
	"github.com/anggasct/aura/internal/platform/embedding"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/secret"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

type knowledgeEmbedder struct {
	client *embedding.Client
}

func (e *knowledgeEmbedder) Embed(ctx context.Context, role knowledge.EmbedRole, texts []string) ([][]float32, error) {
	return e.client.Embed(ctx, embedding.Role(role), texts)
}

func configuredEmbedding(cfg *appconfig.Config) *knowledge.EmbeddingConfig {
	if cfg == nil || cfg.Knowledge == nil {
		return nil
	}
	return cfg.Knowledge.Embedding
}

func resolveEmbeddingCredential(ref secret.Ref) (string, bool) {
	if ref.IsZero() {
		return "", true
	}
	value, err := ref.Reference().Resolve()
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

func newKnowledgeEmbedding(cfg *appconfig.Config, resolver egress.Resolver) (knowledgeEmbeddingWiring, error) {
	embeddingConfig := configuredEmbedding(cfg)
	if embeddingConfig == nil {
		return knowledgeEmbeddingWiring{}, nil
	}
	clientCfg := embeddingConfig.ClientConfig()
	fingerprint, err := embedding.Fingerprint(&clientCfg)
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
	client, err := embedding.New(&clientCfg, credential, resolver)
	if err != nil {
		return knowledgeEmbeddingWiring{}, err
	}
	wiring.embedder = &knowledgeEmbedder{client: client}
	return wiring, nil
}

func knowledgeEmbeddingReader(cfg *appconfig.Config, dbPath string) func(ctx context.Context) (health.KnowledgeEmbeddingState, error) {
	return func(ctx context.Context) (health.KnowledgeEmbeddingState, error) {
		var state health.KnowledgeEmbeddingState
		fingerprint := ""
		dimensions := 0
		if embeddingConfig := configuredEmbedding(cfg); embeddingConfig != nil {
			clientCfg := embeddingConfig.ClientConfig()
			computed, err := embedding.Fingerprint(&clientCfg)
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
		db, err := sqlite.OpenReadOnly(ctx, dbPath)
		if err != nil {
			return health.KnowledgeEmbeddingState{}, err
		}
		defer func() { _ = db.Close() }()
		present, err := knowledge.EmbeddingPresent(ctx, db)
		if err != nil || !present {
			return state, err
		}
		kbStore, err := knowledge.NewSQLiteStore(db)
		if err != nil {
			return health.KnowledgeEmbeddingState{}, err
		}
		coverage, err := kbStore.EmbeddingCoverage(ctx, fingerprint)
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
