package app

import (
	"context"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/platform/egress"
	"github.com/anggasct/aura/internal/platform/embedding"
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
	credential, resolved := knowledge.ResolveEmbeddingCredential(embeddingConfig.CredentialRef)
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
