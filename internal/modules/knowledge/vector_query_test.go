package knowledge

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/principal"
)

type embedCall struct {
	role  EmbedRole
	texts []string
}

type countingEmbedder struct {
	mu     sync.Mutex
	calls  []embedCall
	vector []float32
	err    error
	block  chan struct{}
}

func (e *countingEmbedder) Embed(ctx context.Context, role EmbedRole, texts []string) ([][]float32, error) {
	e.mu.Lock()
	e.calls = append(e.calls, embedCall{role: role, texts: slices.Clone(texts)})
	vector, err, block := e.vector, e.err, e.block
	e.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	return [][]float32{vector}, nil
}

func (e *countingEmbedder) sent() []embedCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.calls)
}

type fakeView struct {
	size    int
	hits    []Hit
	err     error
	queries [][]float32
}

func (v *fakeView) Len() int { return v.size }

func (v *fakeView) Search(_ context.Context, query []float32, _ int) ([]Hit, error) {
	v.queries = append(v.queries, query)
	return v.hits, v.err
}

type fakeVectorIndex struct {
	view   *fakeView
	err    error
	scopes [][]string
}

func (f *fakeVectorIndex) Current(_ context.Context, collections []string) (VectorView, error) {
	f.scopes = append(f.scopes, collections)
	if f.err != nil {
		return nil, f.err
	}
	return f.view, nil
}

type vectorRig struct {
	retriever *Retriever
	embedder  *countingEmbedder
	vectors   *fakeVectorIndex
	lexical   *fakeIndex
	secrets   *fakeSecrets
}

func vectorSettings(publicQueries bool) *EmbeddingSettings {
	return &EmbeddingSettings{
		Fingerprint: "fp", Destination: "api.example.com", Model: "m", Dimensions: 4, BatchSize: 96,
		MaxMemory: 1 << 20, PublicQueries: publicQueries,
	}
}

func newVectorRig(t *testing.T, publicQueries bool, screen SecretScreen, lexical *fakeIndex, view *fakeView, opts ...RetrieverOption) *vectorRig {
	t.Helper()
	rig := &vectorRig{
		embedder: &countingEmbedder{vector: []float32{1, 0, 0, 0}},
		vectors:  &fakeVectorIndex{view: view},
		lexical:  lexical,
		secrets:  &fakeSecrets{screen: screen},
	}
	options := append([]RetrieverOption{WithVectors(&VectorConfig{Index: rig.vectors, Embedder: rig.embedder, Settings: vectorSettings(publicQueries)})}, opts...)
	retriever, err := NewRetriever(lexical, rig.secrets, options...)
	if err != nil {
		t.Fatalf("NewRetriever: %v", err)
	}
	rig.retriever = retriever
	return rig
}

func (r *vectorRig) search(t *testing.T, class principal.Class, query string) Retrieval {
	t.Helper()
	found, err := r.retriever.Search(t.Context(), &SearchRequest{Scope: []string{"support"}, Query: query, Depth: SearchDepth, Class: class})
	if err != nil {
		t.Fatalf("Search(%q): %v", query, err)
	}
	return found
}

func lexicalIndex(hits ...Hit) *fakeIndex {
	return &fakeIndex{chunks: 10, counts: map[string]int{"refund": 2, "policy": 2}, hits: hits}
}

func supportHit(id, content string) Hit {
	return Hit{ChunkID: id, Collection: "support", Label: "faq.md", Content: content}
}

func TestSearchSendsNoQueryUnlessEveryConditionHolds(t *testing.T) {
	loaded := func() *fakeView { return &fakeView{size: 1, hits: []Hit{supportHit("vec", "semantic answer")}} }
	tests := map[string]struct {
		class     principal.Class
		public    bool
		view      func() *fakeView
		indexErr  error
		query     string
		screen    SecretScreen
		wantSends int
		wantMode  string
	}{
		"owner":                       {class: principal.ClassOwner, view: loaded, query: "refund policy", wantSends: 1, wantMode: ModeHybrid},
		"trusted":                     {class: principal.ClassTrusted, view: loaded, query: "refund policy", wantSends: 1, wantMode: ModeHybrid},
		"public by default":           {class: principal.ClassPublic, view: loaded, query: "refund policy", wantSends: 0, wantMode: ModeLexical},
		"public when allowed":         {class: principal.ClassPublic, public: true, view: loaded, query: "refund policy", wantSends: 1, wantMode: ModeHybrid},
		"empty class":                 {class: "", view: loaded, query: "refund policy", wantSends: 0, wantMode: ModeLexical},
		"empty class with public on":  {class: "", public: true, view: loaded, query: "refund policy", wantSends: 0, wantMode: ModeLexical},
		"unknown class":               {class: "administrator", public: true, view: loaded, query: "refund policy", wantSends: 0, wantMode: ModeLexical},
		"no loaded entry":             {class: principal.ClassOwner, view: func() *fakeView { return &fakeView{} }, query: "refund policy", wantSends: 0, wantMode: ModeLexical},
		"version read failed":         {class: principal.ClassOwner, view: loaded, indexErr: errors.New("busy"), query: "refund policy", wantSends: 0, wantMode: ModeLexical},
		"vault value in the query":    {class: principal.ClassOwner, view: loaded, query: "refund policy vault-secret-value", screen: fakeVault{values: []string{"vault-secret-value"}}, wantSends: 0, wantMode: ModeLexical},
		"credential pattern":          {class: principal.ClassOwner, view: loaded, query: "refund ghp_" + strings.Repeat("a", 36), wantSends: 0, wantMode: ModeLexical},
		"vault value beyond the cut":  {class: principal.ClassOwner, view: loaded, query: strings.Repeat("refund ", 200) + "vault-secret-value", screen: fakeVault{values: []string{"vault-secret-value"}}, wantSends: 0, wantMode: ModeLexical},
		"hidden characters in secret": {class: principal.ClassOwner, view: loaded, query: "refund vault-sec\u200bret-value", screen: fakeVault{values: []string{"vault-secret-value"}}, wantSends: 0, wantMode: ModeLexical},
		"blank query":                 {class: principal.ClassOwner, view: loaded, query: " \u200b\n ", wantSends: 0, wantMode: ModeLexical},
		"oversized query":             {class: principal.ClassOwner, view: loaded, query: strings.Repeat("refund ", MaxEmbedQueryBytes), wantSends: 0, wantMode: ModeLexical},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			rig := newVectorRig(t, test.public, test.screen, lexicalIndex(supportHit("lex", "refund policy text")), test.view())
			rig.vectors.err = test.indexErr
			found := rig.search(t, test.class, test.query)
			if got := len(rig.embedder.sent()); got != test.wantSends {
				t.Errorf("query sends = %d, want %d", got, test.wantSends)
			}
			if found.Mode != test.wantMode {
				t.Errorf("mode = %q, want %q", found.Mode, test.wantMode)
			}
			lexicalKept := slices.ContainsFunc(found.Hits, func(hit Hit) bool { return hit.ChunkID == "lex" })
			if lexicalKept != strings.Contains(test.query, "refund") {
				t.Errorf("lexical hit kept = %v for query %q", lexicalKept, test.query)
			}
		})
	}
}

func TestSearchFindsAChunkByMeaningAlone(t *testing.T) {
	semantic := supportHit("vec", "We give your money back within thirty days")
	tests := map[string]struct {
		query string
		index *fakeIndex
	}{
		"no lexical match":      {query: "how do I get my money back", index: &fakeIndex{chunks: 10, counts: map[string]int{}}},
		"no lexical terms left": {query: "?! \u2014 ...", index: &fakeIndex{chunks: 10, counts: map[string]int{}}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			rig := newVectorRig(t, false, nil, test.index, &fakeView{size: 1, hits: []Hit{semantic}})
			found := rig.search(t, principal.ClassOwner, test.query)
			if found.Mode != ModeHybrid || len(found.Hits) != 1 || found.Hits[0].ChunkID != "vec" {
				t.Fatalf("found = %+v, want the semantic chunk in hybrid mode", found)
			}
			if len(rig.embedder.sent()) != 1 {
				t.Errorf("query sends = %d, want 1", len(rig.embedder.sent()))
			}
			if len(test.index.searchTerms) != 0 {
				t.Errorf("lexical search ran with terms %v", test.index.searchTerms)
			}
		})
	}
}

func TestSearchFusesBothListsAndCutsToTheRequestedDepth(t *testing.T) {
	lexical := lexicalIndex(supportHit("a", "refund"), supportHit("b", "refund"))
	rig := newVectorRig(t, false, nil, lexical, &fakeView{size: 1, hits: []Hit{supportHit("b", "refund"), supportHit("c", "refund")}})
	found, err := rig.retriever.Search(t.Context(), &SearchRequest{Scope: []string{"support"}, Query: "refund", Depth: 2, Class: principal.ClassOwner})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := hitIDs(found.Hits); !slices.Equal(got, []string{"b", "a"}) || found.Mode != ModeHybrid {
		t.Fatalf("hits = %v mode %q, want b then a in hybrid mode", got, found.Mode)
	}
}

func TestSearchScreensAHitFoundByVectorsAlone(t *testing.T) {
	view := &fakeView{size: 1, hits: []Hit{
		supportHit("kept", "harmless text"),
		{ChunkID: "stray", Collection: "other", Content: "from another collection"},
		supportHit("vault", "contains vault-secret-value"),
	}}
	rig := newVectorRig(t, false, fakeVault{values: []string{"vault-secret-value"}}, &fakeIndex{chunks: 10}, view)
	found := rig.search(t, principal.ClassOwner, "anything at all")
	if got := hitIDs(found.Hits); !slices.Equal(got, []string{"kept"}) {
		t.Fatalf("hits = %v, want only the harmless in-scope chunk", got)
	}
}

func TestSearchFallsBackToLexicalWhenTheQueryEmbeddingFails(t *testing.T) {
	tests := map[string]func(*countingEmbedder){
		"provider error": func(e *countingEmbedder) { e.err = errors.New("provider down") },
		"wrong width":    func(e *countingEmbedder) { e.vector = []float32{1, 0} },
		"zero vector":    func(e *countingEmbedder) { e.vector = []float32{0, 0, 0, 0} },
		"not a number":   func(e *countingEmbedder) { e.vector = []float32{float32(math.NaN()), 0, 0, 0} },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			view := &fakeView{size: 1, hits: []Hit{supportHit("vec", "never used")}}
			rig := newVectorRig(t, false, nil, lexicalIndex(supportHit("lex", "refund")), view)
			mutate(rig.embedder)
			found := rig.search(t, principal.ClassOwner, "refund")
			if found.Mode != ModeLexicalFallback || len(found.Hits) != 1 || found.Hits[0].ChunkID != "lex" {
				t.Fatalf("found = %+v, want the lexical hit in fallback mode", found)
			}
			if len(view.queries) != 0 {
				t.Errorf("the index was searched with a query that could not be used")
			}
		})
	}
}

func TestSearchFallsBackToLexicalWhenTheVectorSearchFails(t *testing.T) {
	view := &fakeView{size: 1, err: errors.New("lookup failed")}
	rig := newVectorRig(t, false, nil, lexicalIndex(supportHit("lex", "refund")), view)
	found := rig.search(t, principal.ClassOwner, "refund")
	if found.Mode != ModeLexicalFallback || len(found.Hits) != 1 {
		t.Fatalf("found = %+v", found)
	}
}

func TestSearchStopsWaitingForTheEmbeddingAfterItsLimit(t *testing.T) {
	rig := newVectorRig(t, false, nil, lexicalIndex(supportHit("lex", "refund")), &fakeView{size: 1},
		WithEmbedTimeout(20*time.Millisecond))
	rig.embedder.block = make(chan struct{})
	started := time.Now()
	found := rig.search(t, principal.ClassOwner, "refund")
	if found.Mode != ModeLexicalFallback || len(found.Hits) != 1 {
		t.Fatalf("found = %+v, want lexical hits after the timeout", found)
	}
	if len(rig.embedder.sent()) != 1 {
		t.Errorf("query sends = %d, want 1", len(rig.embedder.sent()))
	}
	if time.Since(started) > QueryEmbedTimeout {
		t.Errorf("search waited %s", time.Since(started))
	}
}

func TestSearchRunsTheLexicalSearchWhileTheEmbeddingIsInFlight(t *testing.T) {
	lexical := lexicalIndex(supportHit("lex", "refund"))
	started := make(chan struct{})
	rig := newVectorRig(t, false, nil, nil, &fakeView{size: 1, hits: []Hit{supportHit("vec", "refund")}})
	signaling := &signalingIndex{fakeIndex: lexical, started: started}
	retriever, err := NewRetriever(signaling, rig.secrets, WithVectors(&VectorConfig{Index: rig.vectors, Embedder: rig.embedder, Settings: vectorSettings(false)}))
	if err != nil {
		t.Fatalf("NewRetriever: %v", err)
	}
	rig.embedder.block = started
	found, err := retriever.Search(t.Context(), &SearchRequest{Scope: []string{"support"}, Query: "refund", Depth: SearchDepth, Class: principal.ClassOwner})
	if err != nil || found.Mode != ModeHybrid {
		t.Fatalf("found = %+v, %v, want the embedding to wait for the lexical search and still succeed", found, err)
	}
}

type signalingIndex struct {
	*fakeIndex
	once    sync.Once
	started chan struct{}
}

func (s *signalingIndex) Search(ctx context.Context, scope, terms []string, depth int) ([]Hit, error) {
	s.once.Do(func() { close(s.started) })
	return s.fakeIndex.Search(ctx, scope, terms, depth)
}

func TestSearchSendsTheScreenedNormalizedTextUncut(t *testing.T) {
	query := "re\u200bfund " + strings.Repeat("policy ", 200) + "TAIL\r\nend"
	lexical := lexicalIndex(supportHit("lex", "refund"))
	rig := newVectorRig(t, false, nil, lexical, &fakeView{size: 1})
	rig.search(t, principal.ClassOwner, query)

	calls := rig.embedder.sent()
	if len(calls) != 1 || calls[0].role != EmbedRoleQuery || len(calls[0].texts) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	if want := Normalize(query, FormatText); calls[0].texts[0] != want || !strings.Contains(calls[0].texts[0], "TAIL") {
		t.Errorf("sent %q, want exactly the normalized text", calls[0].texts[0])
	}
	for _, terms := range lexical.searchTerms {
		if slices.Contains(terms, "tail") {
			t.Errorf("the lexical terms reached past the cut: %v", terms)
		}
	}
}

func TestSearchRequestMustNotBeNilOrOutOfRange(t *testing.T) {
	rig := newVectorRig(t, false, nil, lexicalIndex(), &fakeView{size: 1})
	if _, err := rig.retriever.Search(t.Context(), nil); err == nil {
		t.Error("nil request accepted")
	}
	for _, depth := range []int{0, SearchDepth + 1} {
		if _, err := rig.retriever.Search(t.Context(), &SearchRequest{Scope: []string{"support"}, Query: "x", Depth: depth, Class: principal.ClassOwner}); err == nil {
			t.Errorf("depth %d accepted", depth)
		}
	}
	if len(rig.embedder.sent()) != 0 {
		t.Errorf("a rejected request sent %d queries", len(rig.embedder.sent()))
	}
}

func TestSearchWithoutVectorsIsLexicalForEveryClass(t *testing.T) {
	retriever, _ := newRetriever(t, lexicalIndex(supportHit("lex", "refund")), nil)
	for _, class := range []principal.Class{principal.ClassOwner, principal.ClassTrusted, principal.ClassPublic, ""} {
		found, err := retriever.Search(t.Context(), &SearchRequest{Scope: []string{"support"}, Query: "refund", Depth: SearchDepth, Class: class})
		if err != nil || found.Mode != ModeLexical || len(found.Hits) != 1 {
			t.Errorf("class %q: found = %+v, %v", class, found, err)
		}
	}
}

func TestSearchSurfacesALexicalErrorEvenWhenVectorsWork(t *testing.T) {
	boom := errors.New("index down")
	rig := newVectorRig(t, false, nil, &fakeIndex{err: boom}, &fakeView{size: 1, hits: []Hit{supportHit("vec", "x")}})
	if _, err := rig.retriever.Search(t.Context(), &SearchRequest{Scope: []string{"support"}, Query: "refund", Depth: SearchDepth, Class: principal.ClassOwner}); !errors.Is(err, boom) {
		t.Errorf("error = %v, want the lexical error", err)
	}
}

func TestSearchFailsClosedOnAVaultFailureButStillAnswersWithoutHits(t *testing.T) {
	view := &fakeView{size: 1, hits: []Hit{supportHit("vec", "x")}}
	rig := newVectorRig(t, false, nil, &fakeIndex{chunks: 10}, view)
	rig.secrets.err = errors.New("env missing")
	found, err := rig.retriever.Search(t.Context(), &SearchRequest{Scope: []string{"support"}, Query: "anything", Depth: SearchDepth, Class: principal.ClassOwner})
	if err != nil || found.Mode != ModeLexical || len(found.Hits) != 0 || len(rig.embedder.sent()) != 0 {
		t.Fatalf("found = %+v, %v, sends %d, want nothing sent and no hits", found, err, len(rig.embedder.sent()))
	}
	if rig.secrets.calls != 1 {
		t.Errorf("the vault was read %d times, want once", rig.secrets.calls)
	}
}

func TestWithVectorsRejectsIncompleteConfiguration(t *testing.T) {
	good := &VectorConfig{Index: &fakeVectorIndex{view: &fakeView{}}, Embedder: &countingEmbedder{}, Settings: vectorSettings(false)}
	tests := map[string]*VectorConfig{
		"nil":          nil,
		"no index":     {Embedder: good.Embedder, Settings: good.Settings},
		"no embedder":  {Index: good.Index, Settings: good.Settings},
		"no settings":  {Index: good.Index, Embedder: good.Embedder},
		"bad settings": {Index: good.Index, Embedder: good.Embedder, Settings: &EmbeddingSettings{}},
	}
	for name, cfg := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := NewRetriever(&fakeIndex{}, &fakeSecrets{}, WithVectors(cfg)); err == nil {
				t.Error("incomplete vector configuration accepted")
			}
		})
	}
	if _, err := NewRetriever(&fakeIndex{}, &fakeSecrets{}, WithEmbedTimeout(time.Second)); err == nil {
		t.Error("a timeout without vectors accepted")
	}
}
