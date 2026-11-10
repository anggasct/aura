package knowledge

import (
	"context"
	"sync"
	"time"
)

const (
	ModeLexical         = "lexical"
	ModeHybrid          = "hybrid"
	ModeLexicalFallback = "lexical_fallback"

	vectorValueBytes       = 4
	vectorRowOverheadBytes = 96

	loadPageSize      = 512
	loadQueueDepth    = 64
	loadRetryInterval = 30 * time.Second
	scoreWorkers      = 4
	scoreCancelRows   = 4096
)

type CollectionVersion struct {
	Collection  string
	Generation  int64
	Fingerprint string
}

type VectorPage struct {
	RowIDs  []int64
	Digests []string
	Values  []float32
}

type VectorChunk struct {
	RowID  int64
	SHA256 string
	Hit    Hit
}

type VectorReader interface {
	Versions(ctx context.Context, collections []string) ([]CollectionVersion, error)
	Chunks(ctx context.Context, rowIDs []int64) ([]VectorChunk, error)
}

type VectorLoader interface {
	Versions(ctx context.Context, collections []string) ([]CollectionVersion, error)
	EmbeddingState(ctx context.Context, collection, fingerprint string) (EmbeddingState, error)
	Page(ctx context.Context, collection, fingerprint string, dimensions int, afterRowID int64, limit int) (VectorPage, error)
}

type VectorView interface {
	Len() int
	Search(ctx context.Context, query []float32, depth int) ([]Hit, error)
}

type VectorIndex interface {
	Current(ctx context.Context, collections []string) (VectorView, error)
}

func VectorBytes(chunks, dimensions int) int64 {
	return int64(chunks) * (int64(dimensions)*vectorValueBytes + vectorRowOverheadBytes)
}

type MemoryIndexConfig struct {
	Reader      VectorReader
	Loader      VectorLoader
	Fingerprint string
	Dimensions  int
	MaxMemory   int64
	Now         func() time.Time
}

type vectorEntry struct {
	collection string
	generation int64
	rowIDs     []int64
	digests    []string
	values     []float32
	bytes      int64
}

type loadRequest struct {
	collection string
	generation int64
}

type rejection struct {
	generation int64
	need       int64
	until      time.Time
}

type MemoryIndex struct {
	reader      VectorReader
	loader      VectorLoader
	fingerprint string
	dimensions  int
	limit       int64
	now         func() time.Time
	queue       chan loadRequest

	mu       sync.Mutex
	entries  map[string]*vectorEntry
	pending  map[string]struct{}
	rejected map[string]rejection
	used     int64
	reserved int64
	idle     chan struct{}
}

func NewMemoryIndex(cfg *MemoryIndexConfig) (*MemoryIndex, error) {
	if cfg == nil || cfg.Reader == nil || cfg.Loader == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "reader and loader are required")
	}
	if cfg.Fingerprint == "" || cfg.Dimensions <= 0 || cfg.MaxMemory <= 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "fingerprint, dimensions and a memory limit are required")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	idle := make(chan struct{})
	close(idle)
	return &MemoryIndex{
		reader: cfg.Reader, loader: cfg.Loader, fingerprint: cfg.Fingerprint, dimensions: cfg.Dimensions,
		limit: cfg.MaxMemory, now: now, queue: make(chan loadRequest, loadQueueDepth),
		entries: map[string]*vectorEntry{}, pending: map[string]struct{}{}, rejected: map[string]rejection{}, idle: idle,
	}, nil
}

type emptyView struct{}

func (emptyView) Len() int { return 0 }

func (emptyView) Search(context.Context, []float32, int) ([]Hit, error) { return nil, nil }

func (m *MemoryIndex) Current(ctx context.Context, collections []string) (VectorView, error) {
	versions, err := m.reader.Versions(ctx, collections)
	if err != nil {
		return emptyView{}, err
	}
	known := make(map[string]CollectionVersion, len(versions))
	for i := range versions {
		known[versions[i].Collection] = versions[i]
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current := make([]*vectorEntry, 0, len(collections))
	for _, collection := range collections {
		version, found := known[collection]
		if !found || version.Fingerprint != m.fingerprint {
			m.dropLocked(collection)
			delete(m.rejected, collection)
			continue
		}
		if entry := m.entries[collection]; entry != nil {
			if entry.generation == version.Generation {
				current = append(current, entry)
				continue
			}
			m.dropLocked(collection)
		}
		m.requestLoadLocked(collection, version.Generation)
	}
	if len(current) == 0 {
		return emptyView{}, nil
	}
	return &memoryView{index: m, entries: current}, nil
}

func (m *MemoryIndex) dropLocked(collection string) {
	if entry := m.entries[collection]; entry != nil {
		m.used -= entry.bytes
		delete(m.entries, collection)
	}
}

func (m *MemoryIndex) requestLoadLocked(collection string, generation int64) {
	if _, loading := m.pending[collection]; loading {
		return
	}
	if blocked, ok := m.rejected[collection]; ok {
		if blocked.holds(generation, m) {
			return
		}
		delete(m.rejected, collection)
	}
	select {
	case m.queue <- loadRequest{collection: collection, generation: generation}:
		if len(m.pending) == 0 {
			m.idle = make(chan struct{})
		}
		m.pending[collection] = struct{}{}
	default:
	}
}

func (r rejection) holds(generation int64, m *MemoryIndex) bool {
	switch {
	case r.generation != generation:
		return false
	case !r.until.IsZero():
		return m.now().Before(r.until)
	case r.need > 0:
		return m.used+m.reserved+r.need > m.limit
	default:
		return true
	}
}

func (m *MemoryIndex) Used() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.used
}

func (m *MemoryIndex) Idle(ctx context.Context) error {
	m.mu.Lock()
	idle := m.idle
	m.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *MemoryIndex) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case request := <-m.queue:
			m.load(ctx, request)
		}
	}
}
