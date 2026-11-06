package cli

import (
	"context"
	"database/sql"
	"errors"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/knowledge"
	"github.com/anggasct/aura/internal/store"
)

const knowledgeLoaderConns = 1

type knowledgeVectorSource struct {
	reader store.KBVectorReader
	state  *knowledgeVectors
}

func newKnowledgeVectorSource(db *sql.DB) *knowledgeVectorSource {
	return &knowledgeVectorSource{reader: store.NewKBVectorReader(db), state: &knowledgeVectors{store: store.NewKBEmbeddingStore(db)}}
}

func (s *knowledgeVectorSource) Versions(ctx context.Context, collections []string) ([]knowledge.CollectionVersion, error) {
	versions, err := s.reader.CollectionVersions(ctx, collections)
	if err != nil {
		return nil, err
	}
	out := make([]knowledge.CollectionVersion, 0, len(versions))
	for i := range versions {
		out = append(out, knowledge.CollectionVersion{Collection: versions[i].Collection, Generation: versions[i].Generation, Fingerprint: versions[i].Fingerprint})
	}
	return out, nil
}

func (s *knowledgeVectorSource) Chunks(ctx context.Context, rowIDs []int64) ([]knowledge.VectorChunk, error) {
	chunks, err := s.reader.ChunksByRowID(ctx, rowIDs)
	if err != nil {
		return nil, err
	}
	out := make([]knowledge.VectorChunk, 0, len(chunks))
	for i := range chunks {
		out = append(out, knowledge.VectorChunk{
			RowID: chunks[i].RowID, SHA256: chunks[i].SHA256,
			Hit: knowledge.Hit{
				ChunkID: chunks[i].ChunkID, SourceID: chunks[i].SourceID, Collection: chunks[i].Collection,
				Label: chunks[i].Label, Heading: chunks[i].Heading, Ordinal: chunks[i].Ordinal, Content: chunks[i].Content,
			},
		})
	}
	return out, nil
}

func (s *knowledgeVectorSource) EmbeddingState(ctx context.Context, collection, fingerprint string) (knowledge.EmbeddingState, error) {
	return s.state.EmbeddingState(ctx, collection, fingerprint)
}

func (s *knowledgeVectorSource) Page(ctx context.Context, collection, fingerprint string, dimensions int, afterRowID int64, limit int) (knowledge.VectorPage, error) {
	page, err := s.reader.VectorPage(ctx, collection, fingerprint, dimensions, afterRowID, limit)
	if err != nil {
		return knowledge.VectorPage{}, err
	}
	return knowledge.VectorPage{RowIDs: page.RowIDs, Digests: page.Digests, Values: page.Values}, nil
}

type knowledgeVectorRuntime struct {
	index    *knowledge.MemoryIndex
	settings *knowledge.EmbeddingSettings
	embedder knowledge.Embedder
	loader   *sql.DB
	cancel   context.CancelFunc
	done     chan struct{}
}

func startKnowledgeVectors(ctx context.Context, cfg *config.Config, pool *sql.DB, dbPath string) (*knowledgeVectorRuntime, bool, error) {
	wiring, err := newKnowledgeEmbedding(cfg, nil)
	if err != nil {
		return nil, false, err
	}
	if wiring.settings == nil || wiring.embedder == nil {
		return nil, false, nil
	}
	loader, err := store.OpenReadOnly(ctx, dbPath)
	if err != nil {
		return nil, false, err
	}
	loader.SetMaxOpenConns(knowledgeLoaderConns)
	index, err := knowledge.NewMemoryIndex(&knowledge.MemoryIndexConfig{
		Reader: newKnowledgeVectorSource(pool), Loader: newKnowledgeVectorSource(loader),
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
