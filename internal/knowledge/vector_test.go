package knowledge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"slices"
	"sync"
	"testing"
	"time"
)

const (
	matrixFingerprint = "fp-current"
	testDimensions    = 4
	testWait          = 30 * time.Second
)

type fakeCollection struct {
	generation  int64
	fingerprint string
	rowIDs      []int64
	digests     []string
	values      []float32
	covered     int
}

type fakeMatrix struct {
	mu          sync.Mutex
	collections map[string]*fakeCollection
	chunks      map[int64]VectorChunk
	stateCalls  map[string]int
	pageCalls   map[string]int
	versionErr  error
	stateErr    error
	onPage      func(collection string)
}

func newFakeMatrix() *fakeMatrix {
	return &fakeMatrix{
		collections: map[string]*fakeCollection{}, chunks: map[int64]VectorChunk{},
		stateCalls: map[string]int{}, pageCalls: map[string]int{},
	}
}

func unit(values ...float32) []float32 {
	var sum float64
	for _, value := range values {
		sum += float64(value) * float64(value)
	}
	norm := float32(math.Sqrt(sum))
	out := make([]float32, len(values))
	for i, value := range values {
		out[i] = value / norm
	}
	return out
}

func (f *fakeMatrix) addCollection(name string, firstRow int64, vectors ...[]float32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	collection := &fakeCollection{generation: 1, fingerprint: matrixFingerprint, covered: len(vectors)}
	for i, vector := range vectors {
		rowID := firstRow + int64(i)
		digest := fmt.Sprintf("digest-%d", rowID)
		collection.rowIDs = append(collection.rowIDs, rowID)
		collection.digests = append(collection.digests, digest)
		collection.values = append(collection.values, vector...)
		f.chunks[rowID] = VectorChunk{RowID: rowID, SHA256: digest, Hit: Hit{
			ChunkID: fmt.Sprintf("kbc_%d", rowID), Collection: name, Label: name + ".md", Content: fmt.Sprintf("chunk %d", rowID),
		}}
	}
	f.collections[name] = collection
}

func (f *fakeMatrix) update(name string, change func(*fakeCollection)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f.collections[name])
}

func (f *fakeMatrix) pageCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pageCalls[name]
}

func (f *fakeMatrix) stateCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stateCalls[name]
}

func (f *fakeMatrix) Versions(_ context.Context, collections []string) ([]CollectionVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.versionErr != nil {
		return nil, f.versionErr
	}
	var out []CollectionVersion
	for _, name := range collections {
		if collection, ok := f.collections[name]; ok {
			out = append(out, CollectionVersion{Collection: name, Generation: collection.generation, Fingerprint: collection.fingerprint})
		}
	}
	return out, nil
}

func (f *fakeMatrix) Chunks(_ context.Context, rowIDs []int64) ([]VectorChunk, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []VectorChunk
	for _, rowID := range rowIDs {
		if chunk, ok := f.chunks[rowID]; ok {
			out = append(out, chunk)
		}
	}
	return out, nil
}

func (f *fakeMatrix) EmbeddingState(_ context.Context, name, fingerprint string) (EmbeddingState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stateCalls[name]++
	if f.stateErr != nil {
		return EmbeddingState{}, f.stateErr
	}
	collection, ok := f.collections[name]
	if !ok {
		return EmbeddingState{}, nil
	}
	return EmbeddingState{Exists: true, Fingerprint: collection.fingerprint, Chunks: len(collection.rowIDs), Covered: collection.covered}, nil
}

func (f *fakeMatrix) Page(_ context.Context, name, _ string, dimensions int, after int64, limit int) (VectorPage, error) {
	f.mu.Lock()
	hook := f.onPage
	f.pageCalls[name]++
	collection := f.collections[name]
	var page VectorPage
	for i, rowID := range collection.rowIDs {
		if rowID <= after || len(page.RowIDs) == limit {
			continue
		}
		page.RowIDs = append(page.RowIDs, rowID)
		page.Digests = append(page.Digests, collection.digests[i])
		page.Values = append(page.Values, collection.values[i*dimensions:(i+1)*dimensions]...)
	}
	f.mu.Unlock()
	if hook != nil {
		hook(name)
	}
	return page, nil
}

type indexRig struct {
	index *MemoryIndex
	store *fakeMatrix
	now   *time.Time
}

func newIndexRig(t *testing.T, store *fakeMatrix, maxMemory int64) *indexRig {
	t.Helper()
	clock := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	rig := &indexRig{store: store, now: &clock}
	index, err := NewMemoryIndex(&MemoryIndexConfig{
		Reader: store, Loader: store, Fingerprint: matrixFingerprint, Dimensions: testDimensions, MaxMemory: maxMemory,
		Now: func() time.Time { return *rig.now },
	})
	if err != nil {
		t.Fatalf("NewMemoryIndex: %v", err)
	}
	rig.index = index
	runCtx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		index.Run(runCtx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return rig
}

func (r *indexRig) settle(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testWait)
	defer cancel()
	if err := r.index.Idle(ctx); err != nil {
		t.Fatalf("index never went idle: %v", err)
	}
}

func (r *indexRig) current(t *testing.T, collections ...string) VectorView {
	t.Helper()
	view, err := r.index.Current(t.Context(), collections)
	if err != nil {
		t.Fatalf("Current(%v): %v", collections, err)
	}
	return view
}

func (r *indexRig) loaded(t *testing.T, collections ...string) VectorView {
	t.Helper()
	r.current(t, collections...)
	r.settle(t)
	return r.current(t, collections...)
}

func hitIDs(hits []Hit) []string {
	ids := make([]string, len(hits))
	for i := range hits {
		ids[i] = hits[i].ChunkID
	}
	return ids
}

func TestMemoryIndexAnswersLexicallyUntilTheLoaderFinishes(t *testing.T) {
	store := newFakeMatrix()
	store.addCollection("support", 10, unit(1, 0, 0, 0), unit(0, 1, 0, 0), unit(1, 1, 0, 0))
	rig := newIndexRig(t, store, 1<<20)

	if view := rig.current(t, "support"); view.Len() != 0 {
		t.Fatalf("an unloaded collection is current: %d entries", view.Len())
	}
	rig.settle(t)
	view := rig.current(t, "support")
	if view.Len() != 1 {
		t.Fatalf("loaded entries = %d, want 1", view.Len())
	}
	hits, err := view.Search(t.Context(), unit(1, 0.1, 0, 0), 2)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := hitIDs(hits); !slices.Equal(got, []string{"kbc_10", "kbc_12"}) {
		t.Fatalf("hits = %v, want the two nearest chunks", got)
	}
	if got, want := rig.index.Used(), VectorBytes(3, testDimensions); got != want {
		t.Errorf("Used = %d, want %d", got, want)
	}
}

func TestMemoryIndexChecksCoverageBeforeReadingAnyVector(t *testing.T) {
	store := newFakeMatrix()
	store.addCollection("support", 10, unit(1, 0, 0, 0), unit(0, 1, 0, 0))
	store.update("support", func(c *fakeCollection) { c.covered = 1 })
	rig := newIndexRig(t, store, 1<<20)

	for range 3 {
		rig.current(t, "support")
		rig.settle(t)
	}
	if store.pageCount("support") != 0 {
		t.Errorf("a partial collection read %d vector pages", store.pageCount("support"))
	}
	if store.stateCount("support") != 1 {
		t.Errorf("coverage checked %d times for one generation, want once", store.stateCount("support"))
	}
	if rig.current(t, "support").Len() != 0 {
		t.Error("a partial collection joined vector search")
	}

	store.update("support", func(c *fakeCollection) { c.covered = 2; c.generation++ })
	if view := rig.loaded(t, "support"); view.Len() != 1 {
		t.Fatalf("a completed collection did not load: %d entries", view.Len())
	}
}

func TestMemoryIndexDropsAStaleEntryAndReloadsAtTheNewGeneration(t *testing.T) {
	store := newFakeMatrix()
	store.addCollection("support", 10, unit(1, 0, 0, 0), unit(0, 1, 0, 0))
	rig := newIndexRig(t, store, 1<<20)
	if rig.loaded(t, "support").Len() != 1 {
		t.Fatal("collection did not load")
	}

	store.update("support", func(c *fakeCollection) {
		c.generation++
		c.rowIDs, c.digests, c.values = []int64{30}, []string{"digest-30"}, unit(0, 0, 1, 0)
		c.covered = 1
	})
	store.mu.Lock()
	store.chunks[30] = VectorChunk{RowID: 30, SHA256: "digest-30", Hit: Hit{ChunkID: "kbc_30", Collection: "support"}}
	store.mu.Unlock()

	if view := rig.current(t, "support"); view.Len() != 0 {
		t.Fatalf("a stale entry answered the next retrieval: %d entries", view.Len())
	}
	rig.settle(t)
	view := rig.current(t, "support")
	hits, err := view.Search(t.Context(), unit(0, 0, 1, 0), 5)
	if err != nil || !slices.Equal(hitIDs(hits), []string{"kbc_30"}) {
		t.Fatalf("hits = %v, %v, want only the reloaded chunk", hitIDs(hits), err)
	}
}

func TestMemoryIndexNeverComparesVectorsOfAnotherFingerprint(t *testing.T) {
	store := newFakeMatrix()
	store.addCollection("support", 10, unit(1, 0, 0, 0))
	rig := newIndexRig(t, store, 1<<20)
	if rig.loaded(t, "support").Len() != 1 {
		t.Fatal("collection did not load")
	}
	checks := store.stateCount("support")

	store.update("support", func(c *fakeCollection) { c.fingerprint = "fp-other"; c.generation++ })
	if rig.current(t, "support").Len() != 0 || rig.index.Used() != 0 {
		t.Fatalf("an entry survived a fingerprint change: used %d", rig.index.Used())
	}
	rig.settle(t)
	if store.stateCount("support") != checks || store.pageCount("support") != 1 {
		t.Errorf("a collection embedded under another fingerprint was loaded: state %d pages %d", store.stateCount("support"), store.pageCount("support"))
	}
}

func TestMemoryIndexKeepsAnOverLimitCollectionLexicalAndNeverEvicts(t *testing.T) {
	store := newFakeMatrix()
	store.addCollection("small", 10, unit(1, 0, 0, 0))
	store.addCollection("large", 100, unit(1, 0, 0, 0), unit(0, 1, 0, 0), unit(0, 0, 1, 0), unit(0, 0, 0, 1))
	limit := VectorBytes(1, testDimensions) + VectorBytes(2, testDimensions)
	rig := newIndexRig(t, store, limit)

	if rig.loaded(t, "small").Len() != 1 {
		t.Fatal("the small collection did not load")
	}
	for range 3 {
		rig.current(t, "large")
		rig.settle(t)
	}
	if rig.current(t, "large").Len() != 0 {
		t.Error("an over-limit collection joined vector search")
	}
	if rig.current(t, "small").Len() != 1 {
		t.Error("loading was refused by evicting a loaded collection")
	}
	if got := rig.index.Used(); got != VectorBytes(1, testDimensions) || got > limit {
		t.Errorf("Used = %d, limit %d", got, limit)
	}
	if store.pageCount("large") != 0 || store.stateCount("large") != 1 {
		t.Errorf("an over-limit collection was read: pages %d, coverage checks %d", store.pageCount("large"), store.stateCount("large"))
	}
}

func TestMemoryIndexReservesMemoryForLoadsInFlight(t *testing.T) {
	store := newFakeMatrix()
	store.addCollection("first", 10, unit(1, 0, 0, 0), unit(0, 1, 0, 0))
	store.addCollection("second", 100, unit(1, 0, 0, 0), unit(0, 1, 0, 0))
	rig := newIndexRig(t, store, VectorBytes(2, testDimensions)+VectorBytes(1, testDimensions))

	rig.current(t, "first", "second")
	rig.settle(t)
	if view := rig.current(t, "first", "second"); view.Len() != 1 {
		t.Fatalf("both collections loaded into a limit that fits one: %d entries", view.Len())
	}
	if rig.index.Used() > VectorBytes(2, testDimensions)+VectorBytes(1, testDimensions) {
		t.Errorf("Used = %d exceeds the limit", rig.index.Used())
	}
}

func TestMemoryIndexDiscardsAResultThatWentStaleWhileLoading(t *testing.T) {
	store := newFakeMatrix()
	store.addCollection("support", 10, unit(1, 0, 0, 0), unit(0, 1, 0, 0))
	var once sync.Once
	store.onPage = func(name string) {
		once.Do(func() { store.update(name, func(c *fakeCollection) { c.generation++ }) })
	}
	rig := newIndexRig(t, store, 1<<20)

	rig.current(t, "support")
	rig.settle(t)
	if rig.index.Used() != 0 {
		t.Fatalf("a result from before the change was stored: used %d", rig.index.Used())
	}
	if view := rig.loaded(t, "support"); view.Len() != 1 {
		t.Fatalf("the collection did not load at its new generation: %d entries", view.Len())
	}
}

func TestMemoryIndexRetriesATransientFailureOnlyAfterTheInterval(t *testing.T) {
	store := newFakeMatrix()
	store.addCollection("support", 10, unit(1, 0, 0, 0))
	store.stateErr = errors.New("database busy")
	rig := newIndexRig(t, store, 1<<20)

	rig.current(t, "support")
	rig.settle(t)
	rig.current(t, "support")
	rig.settle(t)
	if store.stateCount("support") != 1 {
		t.Fatalf("coverage checked %d times inside the retry interval, want 1", store.stateCount("support"))
	}
	store.mu.Lock()
	store.stateErr = nil
	store.mu.Unlock()
	*rig.now = rig.now.Add(loadRetryInterval + time.Second)
	if view := rig.loaded(t, "support"); view.Len() != 1 {
		t.Fatalf("the collection did not load after the interval: %d entries", view.Len())
	}
}

func TestMemoryIndexKeepsLexicalWhenTheVersionReadFails(t *testing.T) {
	store := newFakeMatrix()
	store.addCollection("support", 10, unit(1, 0, 0, 0))
	store.versionErr = errors.New("database busy")
	rig := newIndexRig(t, store, 1<<20)
	view, err := rig.index.Current(t.Context(), []string{"support"})
	if err == nil || view.Len() != 0 {
		t.Fatalf("Current = %d entries, %v, want an error and nothing current", view.Len(), err)
	}
}

func TestMemoryIndexSearchDropsChunksThatChangedSinceTheLoad(t *testing.T) {
	store := newFakeMatrix()
	store.addCollection("support", 10, unit(1, 0, 0, 0), unit(0.9, 0.1, 0, 0), unit(0.8, 0.2, 0, 0))
	store.addCollection("other", 100, unit(1, 0, 0, 0))
	rig := newIndexRig(t, store, 1<<20)
	view := rig.loaded(t, "support")

	store.mu.Lock()
	changed := store.chunks[10]
	changed.SHA256 = "digest-rewritten"
	store.chunks[10] = changed
	moved := store.chunks[11]
	moved.Hit.Collection = "other"
	store.chunks[11] = moved
	delete(store.chunks, 12)
	store.mu.Unlock()

	hits, err := view.Search(t.Context(), unit(1, 0, 0, 0), 5)
	if err != nil || len(hits) != 0 {
		t.Fatalf("hits = %v, %v, want every changed chunk dropped", hitIDs(hits), err)
	}
}

func TestMemoryIndexSearchRejectsAMisshapenQuery(t *testing.T) {
	store := newFakeMatrix()
	store.addCollection("support", 10, unit(1, 0, 0, 0))
	view := newIndexRig(t, store, 1<<20).loaded(t, "support")
	for name, test := range map[string]struct {
		query []float32
		depth int
	}{
		"short":      {query: []float32{1, 0}, depth: 5},
		"no depth":   {query: unit(1, 0, 0, 0), depth: 0},
		"too deep":   {query: unit(1, 0, 0, 0), depth: SearchDepth + 1},
		"long query": {query: []float32{1, 0, 0, 0, 0}, depth: 5},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := view.Search(t.Context(), test.query, test.depth); err == nil {
				t.Error("Search accepted a misshapen request")
			}
		})
	}
}

func TestMemoryIndexScoresExactlyAcrossWorkers(t *testing.T) {
	random := rand.New(rand.NewSource(7))
	const rows = 2500
	vectors := make([][]float32, rows)
	for i := range vectors {
		vectors[i] = unit(random.Float32()+0.01, random.Float32()+0.01, random.Float32()+0.01, random.Float32()+0.01)
	}
	store := newFakeMatrix()
	store.addCollection("big", 1, vectors...)
	view := newIndexRig(t, store, 1<<30).loaded(t, "big")
	query := unit(0.3, 0.5, 0.1, 0.9)

	type scored struct {
		row   int
		score float32
	}
	want := make([]scored, rows)
	for i := range vectors {
		want[i] = scored{row: i, score: dot(query, vectors[i])}
	}
	slices.SortFunc(want, func(a, b scored) int {
		switch {
		case a.score > b.score:
			return -1
		case a.score < b.score:
			return 1
		default:
			return a.row - b.row
		}
	})
	hits, err := view.Search(t.Context(), query, 20)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for i := range hits {
		if wantID := fmt.Sprintf("kbc_%d", want[i].row+1); hits[i].ChunkID != wantID {
			t.Fatalf("hit %d = %s (%.6f), want %s (%.6f)", i, hits[i].ChunkID, hits[i].Score, wantID, want[i].score)
		}
	}
	if len(hits) != 20 {
		t.Fatalf("hits = %d, want 20", len(hits))
	}
}

func TestTopRowsKeepsTheBestInOrderAndBreaksTiesByRowID(t *testing.T) {
	top := topRows{limit: 3}
	for _, candidate := range []scoredRow{
		{score: 0.5, rowID: 9}, {score: 0.9, rowID: 7}, {score: 0.5, rowID: 2}, {score: 0.1, rowID: 1},
		{score: 0.9, rowID: 3}, {score: 0.5, rowID: 1}, {score: 0.95, rowID: 8},
	} {
		top.offer(&candidate)
	}
	var got []int64
	for i := range top.items {
		got = append(got, top.items[i].rowID)
	}
	if want := []int64{8, 3, 7}; !slices.Equal(got, want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
}

func TestMemoryIndexRunStopsWhenItsContextEnds(t *testing.T) {
	store := newFakeMatrix()
	index, err := NewMemoryIndex(&MemoryIndexConfig{Reader: store, Loader: store, Fingerprint: matrixFingerprint, Dimensions: testDimensions, MaxMemory: 1 << 20})
	if err != nil {
		t.Fatalf("NewMemoryIndex: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		index.Run(ctx)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(testWait):
		t.Fatal("Run did not return after its context ended")
	}
}

func TestNewMemoryIndexRequiresItsDependencies(t *testing.T) {
	store := newFakeMatrix()
	valid := MemoryIndexConfig{Reader: store, Loader: store, Fingerprint: matrixFingerprint, Dimensions: testDimensions, MaxMemory: 1}
	tests := map[string]func(*MemoryIndexConfig){
		"no reader":      func(c *MemoryIndexConfig) { c.Reader = nil },
		"no loader":      func(c *MemoryIndexConfig) { c.Loader = nil },
		"no fingerprint": func(c *MemoryIndexConfig) { c.Fingerprint = "" },
		"no dimensions":  func(c *MemoryIndexConfig) { c.Dimensions = 0 },
		"no memory":      func(c *MemoryIndexConfig) { c.MaxMemory = 0 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			mutate(&cfg)
			if _, err := NewMemoryIndex(&cfg); err == nil {
				t.Error("an incomplete configuration was accepted")
			}
		})
	}
	if _, err := NewMemoryIndex(nil); err == nil {
		t.Error("a nil configuration was accepted")
	}
}
