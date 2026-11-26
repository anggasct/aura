package app

import (
	"context"
	"database/sql"
	"errors"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const knowledgeLoaderConns = 1

type knowledgeVectorRuntime struct {
	index    *knowledge.MemoryIndex
	settings *knowledge.EmbeddingSettings
	embedder knowledge.Embedder
	loader   *sql.DB
	cancel   context.CancelFunc
	done     chan struct{}
}

func startKnowledgeVectors(ctx context.Context, cfg *appconfig.Config, pool *sql.DB, dbPath string) (*knowledgeVectorRuntime, bool, error) {
	wiring, err := newKnowledgeEmbedding(cfg, nil)
	if err != nil {
		return nil, false, err
	}
	if wiring.settings == nil || wiring.embedder == nil {
		return nil, false, nil
	}
	reader, err := knowledge.NewSQLiteVectorSource(pool)
	if err != nil {
		return nil, false, err
	}
	loader, err := sqlite.OpenReadOnly(ctx, dbPath)
	if err != nil {
		return nil, false, err
	}
	loader.SetMaxOpenConns(knowledgeLoaderConns)
	loaderSource, err := knowledge.NewSQLiteVectorSource(loader)
	if err != nil {
		return nil, false, errors.Join(err, loader.Close())
	}
	index, err := knowledge.NewMemoryIndex(&knowledge.MemoryIndexConfig{
		Reader: reader, Loader: loaderSource,
		Fingerprint: wiring.settings.Fingerprint, Dimensions: wiring.settings.Dimensions, MaxMemory: wiring.settings.MaxMemory,
	})
	if err != nil {
		return nil, false, errors.Join(err, loader.Close())
	}
	runCtx, cancel := context.WithCancel(ctx)
	runtime := &knowledgeVectorRuntime{
		index: index, settings: wiring.settings, embedder: wiring.embedder,
		loader: loader, cancel: cancel, done: make(chan struct{}),
	}
	go func() {
		defer close(runtime.done)
		index.Run(runCtx)
	}()
	return runtime, true, nil
}

func (r *knowledgeVectorRuntime) options() []knowledge.RetrieverOption {
	if r == nil {
		return nil
	}
	return []knowledge.RetrieverOption{knowledge.WithVectors(&knowledge.VectorConfig{Index: r.index, Embedder: r.embedder, Settings: r.settings})}
}

func (r *knowledgeVectorRuntime) close() {
	if r == nil {
		return
	}
	r.cancel()
	<-r.done
	_ = r.loader.Close()
}
