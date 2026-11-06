package knowledge

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sync/errgroup"

	"github.com/anggasct/aura/internal/principal"
)

const (
	SearchDepth = 50

	MaxQueryTerms  = 16
	MaxFinalTerms  = 8
	MaxCountSum    = 30000
	MinTermCap     = 1000
	TermCapPercent = 10
	MaxTermRunes   = 64
	MaxQueryRunes  = 500

	countParallelism = 4
)

type Hit struct {
	ChunkID    string
	SourceID   string
	Collection string
	Label      string
	Heading    string
	Ordinal    int
	Content    string
	Score      float64
}

type Index interface {
	ScopeChunks(ctx context.Context, collections []string) (int, error)
	TermFrequency(ctx context.Context, collections []string, term string, limit int) (int, error)
	Search(ctx context.Context, collections []string, terms []string, depth int) ([]Hit, error)
}

type VectorConfig struct {
	Index    VectorIndex
	Embedder Embedder
	Settings *EmbeddingSettings
}

type RetrieverOption func(*Retriever) error

func WithVectors(cfg *VectorConfig) RetrieverOption {
	return func(r *Retriever) error {
		if cfg == nil || cfg.Index == nil || cfg.Embedder == nil || cfg.Settings == nil || !cfg.Settings.valid() {
			return Errorf(ErrorCodeInvalidArgument, "vector retrieval needs an index, an embedder and valid settings")
		}
		r.vectors = &vectorPath{index: cfg.Index, embedder: cfg.Embedder, settings: cfg.Settings, timeout: QueryEmbedTimeout}
		return nil
	}
}

func WithEmbedTimeout(timeout time.Duration) RetrieverOption {
	return func(r *Retriever) error {
		if r.vectors == nil || timeout <= 0 {
			return Errorf(ErrorCodeInvalidArgument, "an embed timeout needs vector retrieval and a positive duration")
		}
		r.vectors.timeout = timeout
		return nil
	}
}

type Retriever struct {
	index   Index
	secrets SecretSource
	vectors *vectorPath
}

func NewRetriever(index Index, secrets SecretSource, opts ...RetrieverOption) (*Retriever, error) {
	if index == nil || secrets == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "index and secrets are required")
	}
	retriever := &Retriever{index: index, secrets: secrets}
	for _, opt := range opts {
		if err := opt(retriever); err != nil {
			return nil, err
		}
	}
	return retriever, nil
}

type SearchRequest struct {
	Scope []string
	Query string
	Depth int
	Class principal.Class
}

type Retrieval struct {
	Hits []Hit
	Mode string
}

func (r *Retriever) Search(ctx context.Context, request *SearchRequest) (Retrieval, error) {
	if request == nil {
		return Retrieval{}, Errorf(ErrorCodeInvalidArgument, "search request must not be nil")
	}
	collections, err := validScope(request.Scope)
	if err != nil {
		return Retrieval{}, err
	}
	if request.Depth <= 0 || request.Depth > SearchDepth {
		return Retrieval{}, Errorf(ErrorCodeInvalidArgument, "depth must be between 1 and %d", SearchDepth)
	}
	vault := &vaultSnapshot{source: r.secrets}
	var lexical []Hit
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		found, err := r.lexicalHits(groupCtx, collections, QueryTerms(request.Query))
		lexical = found
		return err
	})
	semantic, mode := r.vectors.search(groupCtx, collections, request, vault)
	if err := group.Wait(); err != nil {
		return Retrieval{}, err
	}
	hits := lexical
	if mode == ModeHybrid {
		hits = Fuse(lexical, semantic)
	}
	screened, err := r.screen(ctx, hits, collections, vault)
	if err != nil {
		return Retrieval{}, err
	}
	return Retrieval{Hits: screened[:min(len(screened), request.Depth)], Mode: mode}, nil
}

func (r *Retriever) lexicalHits(ctx context.Context, collections, terms []string) ([]Hit, error) {
	if len(terms) == 0 {
		return nil, nil
	}
	chosen, err := r.chooseTerms(ctx, collections, terms)
	if err != nil || len(chosen) == 0 {
		return nil, err
	}
	return r.index.Search(ctx, collections, chosen, SearchDepth)
}

type vaultSnapshot struct {
	source SecretSource
	once   sync.Once
	screen SecretScreen
	err    error
}

func (v *vaultSnapshot) get(ctx context.Context) (SecretScreen, error) {
	v.once.Do(func() { v.screen, v.err = v.source.Snapshot(ctx) })
	return v.screen, v.err
}

func validScope(scope []string) ([]string, error) {
	collections := make([]string, 0, len(scope))
	for _, collection := range scope {
		if err := requireCollection(collection); err != nil {
			return nil, err
		}
		if !slices.Contains(collections, collection) {
			collections = append(collections, collection)
		}
	}
	if len(collections) == 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "scope must list at least one collection")
	}
	return collections, nil
}

func QueryTerms(query string) []string {
	if utf8.RuneCountInString(query) > MaxQueryRunes {
		query = string([]rune(query)[:MaxQueryRunes])
	}
	fields := strings.FieldsFunc(Normalize(query, FormatText), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.IsMark(r)
	})
	terms := make([]string, 0, MaxQueryTerms)
	for _, field := range fields {
		term := strings.ToLower(field)
		if utf8.RuneCountInString(term) > MaxTermRunes || slices.Contains(terms, term) {
			continue
		}
		terms = append(terms, term)
		if len(terms) == MaxQueryTerms {
			break
		}
	}
	return terms
}

type termCount struct {
	term  string
	count int
}

func (r *Retriever) chooseTerms(ctx context.Context, collections, terms []string) ([]string, error) {
	chunks, err := r.index.ScopeChunks(ctx, collections)
	if err != nil {
		return nil, err
	}
	threshold := max(chunks*TermCapPercent/100, MinTermCap)
	frequencies := make([]int, len(terms))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(countParallelism)
	for i, term := range terms {
		group.Go(func() error {
			count, err := r.index.TermFrequency(groupCtx, collections, term, threshold+1)
			frequencies[i] = count
			return err
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	counted := make([]termCount, 0, len(terms))
	for i, term := range terms {
		if frequencies[i] == 0 || frequencies[i] > threshold {
			continue
		}
		counted = append(counted, termCount{term: term, count: frequencies[i]})
	}
	slices.SortFunc(counted, func(a, b termCount) int {
		return cmp.Or(cmp.Compare(a.count, b.count), cmp.Compare(a.term, b.term))
	})
	chosen := make([]string, 0, MaxFinalTerms)
	sum := 0
	for _, entry := range counted {
		if len(chosen) == MaxFinalTerms || (len(chosen) > 0 && sum+entry.count > MaxCountSum) {
			break
		}
		chosen = append(chosen, entry.term)
		sum += entry.count
	}
	return chosen, nil
}

func (r *Retriever) screen(ctx context.Context, hits []Hit, collections []string, snapshot *vaultSnapshot) ([]Hit, error) {
	if len(hits) == 0 {
		return nil, nil
	}
	vault, err := snapshot.get(ctx)
	if err != nil {
		return nil, Errorf(ErrorCodeSecretUnavailable, "a configured secret reference could not be resolved")
	}
	kept := make([]Hit, 0, len(hits))
	for i := range hits {
		hit := &hits[i]
		if !slices.Contains(collections, hit.Collection) {
			continue
		}
		if vault != nil && vault.Contains(hit.Label+"\n"+hit.Heading+"\n"+hit.Content) {
			continue
		}
		kept = append(kept, *hit)
	}
	return kept, nil
}
