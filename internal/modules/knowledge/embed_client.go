package knowledge

import (
	"context"

	"github.com/anggasct/aura/internal/platform/egress"
	"github.com/anggasct/aura/internal/platform/embedding"
	"github.com/anggasct/aura/internal/platform/secret"
)

type clientEmbedder struct {
	client *embedding.Client
}

func (e *clientEmbedder) Embed(ctx context.Context, role EmbedRole, texts []string) ([][]float32, error) {
	return e.client.Embed(ctx, embedding.Role(role), texts)
}

func ResolveEmbeddingCredential(ref secret.Ref) (string, bool) {
	return embedding.ResolveCredential(ref)
}

type EmbeddingWiring struct {
	Settings *EmbeddingSettings
	Embedder Embedder
}

func NewEmbeddingWiring(cfg *Config, resolver egress.Resolver) (EmbeddingWiring, error) {
	if cfg == nil || cfg.Embedding == nil {
		return EmbeddingWiring{}, nil
	}
	embeddingConfig := cfg.Embedding
	clientCfg := embeddingConfig.ClientConfig()
	fingerprint, err := embedding.Fingerprint(&clientCfg)
	if err != nil {
		return EmbeddingWiring{}, err
	}
	endpoint, err := embeddingConfig.Endpoint()
	if err != nil {
		return EmbeddingWiring{}, err
	}
	wiring := EmbeddingWiring{Settings: &EmbeddingSettings{
		Fingerprint: fingerprint, Destination: endpoint.Authority(), Loopback: endpoint.Loopback,
		Model: embeddingConfig.Model, Dimensions: embeddingConfig.Dimensions, BatchSize: embeddingConfig.BatchSize,
		MaxMemory: int64(embeddingConfig.MaxMemory), PublicQueries: embeddingConfig.PublicQueries,
	}}
	credential, resolved := ResolveEmbeddingCredential(embeddingConfig.CredentialRef)
	if !resolved {
		return wiring, nil
	}
	client, err := embedding.New(&clientCfg, credential, resolver)
	if err != nil {
		return EmbeddingWiring{}, err
	}
	wiring.Embedder = &clientEmbedder{client: client}
	return wiring, nil
}
