package profile

import (
	stdcontext "context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/anggasct/aura/internal/platform/embedding"
	"github.com/anggasct/aura/internal/platform/secret"
)

const (
	QueryEmbedTimeout  = embedding.QueryEmbedTimeout
	MaxEmbedQueryBytes = embedding.MaxEmbedQueryBytes

	defaultEmbedBatchSize    = 96
	defaultBackfillInterval  = 30 * time.Second
	maxBackfillRowsPerTick   = 200
	maxVectorScanRows        = 10000
	vectorScanCancelInterval = 256
	postWriteEmbedTimeout    = 10 * time.Second

	semanticCosineWeight  = 0.85
	semanticRecencyWeight = 0.15
)

type Embedder interface {
	Embed(ctx stdcontext.Context, role embedding.Role, texts []string) ([][]float32, error)
}

type SecretSource interface {
	Snapshot(ctx stdcontext.Context) (secret.Screen, error)
}

type EmbeddingSettings struct {
	Fingerprint   string
	Dimensions    int
	BatchSize     int
	PublicQueries bool
	QueryTimeout  time.Duration
}

func (e *EmbeddingSettings) valid() bool {
	return e != nil && e.Fingerprint != "" && e.Dimensions > 0
}

func (e *EmbeddingSettings) batchSize() int {
	if e == nil || e.BatchSize <= 0 {
		return defaultEmbedBatchSize
	}
	return min(e.BatchSize, embedding.MaxBatchSize)
}

func (e *EmbeddingSettings) queryTimeout() time.Duration {
	if e == nil || e.QueryTimeout <= 0 {
		return QueryEmbedTimeout
	}
	return e.QueryTimeout
}

type EmbeddingOptions struct {
	Enabled           bool
	Embedder          Embedder
	Settings          *EmbeddingSettings
	Scanner           SecretScanner
	SecretSource      SecretSource
	MaxVectorScanRows int
}

type EmbeddingCoverage struct {
	Total    int
	Embedded int
	Stale    int
}

type factEmbedder interface {
	EmbedFactBestEffort(ctx stdcontext.Context, fact *Fact)
}

func factEmbedText(key, value string) string {
	return key + ": " + value
}

func factContentSHA256(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
