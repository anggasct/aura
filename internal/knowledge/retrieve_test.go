package knowledge

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

type fakeIndex struct {
	mu          sync.Mutex
	chunks      int
	counts      map[string]int
	hits        []Hit
	searchTerms [][]string
	counted     []string
	limits      []int
	scopes      [][]string
	err         error
}

func (f *fakeIndex) ScopeChunks(context.Context, []string) (int, error) { return f.chunks, f.err }

func (f *fakeIndex) TermFrequency(_ context.Context, scope []string, term string, limit int) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	f.counted = append(f.counted, term)
	f.limits = append(f.limits, limit)
	f.scopes = append(f.scopes, scope)
	return min(f.counts[term], limit), nil
}

func (f *fakeIndex) Search(_ context.Context, scope, terms []string, depth int) ([]Hit, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.scopes = append(f.scopes, scope)
	f.searchTerms = append(f.searchTerms, terms)
	if depth != SearchDepth {
		return nil, fmt.Errorf("depth = %d", depth)
	}
	return f.hits, nil
}

func newRetriever(t *testing.T, index *fakeIndex, screen SecretScreen) (*Retriever, *fakeSecrets) {
	t.Helper()
	secrets := &fakeSecrets{screen: screen}
	retriever, err := NewRetriever(index, secrets)
	if err != nil {
		t.Fatalf("NewRetriever: %v", err)
	}
	return retriever, secrets
}

func TestQueryTermsBoundNormalizeAndDeduplicate(t *testing.T) {
	long := strings.Repeat("a", MaxTermRunes+1)
	var many []string
	for i := range 40 {
		many = append(many, fmt.Sprintf("word%d", i))
	}
	tests := map[string]struct {
		query string
		want  []string
	}{
		"question":        {query: "What is your refund policy?", want: []string{"what", "is", "your", "refund", "policy"}},
		"empty":           {query: "  \n ", want: nil},
		"punctuation":     {query: `"?!-- ()`, want: nil},
		"case and repeat": {query: "Refund REFUND refund", want: []string{"refund"}},
		"syntax":          {query: `a" OR scope:x NEAR(b c) *`, want: []string{"a", "or", "scope", "x", "near", "b", "c"}},
		"hidden":          {query: "re\u200bfund\u202e policy", want: []string{"refund", "policy"}},
		"diacritics":      {query: "Café Zoë", want: []string{"café", "zoë"}},
		"long term":       {query: long + " ok", want: []string{"ok"}},
		"capped":          {query: strings.Join(many, " "), want: many[:MaxQueryTerms]},
		"digits":          {query: "order 66b", want: []string{"order", "66b"}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if got := QueryTerms(test.query); !slices.Equal(got, test.want) {
				t.Errorf("QueryTerms(%q) = %q, want %q", test.query, got, test.want)
			}
		})
	}
	if got := QueryTerms(strings.Repeat("x ", MaxQueryRunes) + "tail"); slices.Contains(got, "tail") {
		t.Errorf("query beyond %d runes was not cut: %q", MaxQueryRunes, got)
	}
}

func TestSearchChoosesTheRarestTermsInsideTheBudget(t *testing.T) {
	counts := map[string]int{}
	var words []string
	for i, count := range []int{900, 20, 20, 5, 700, 3, 3, 1, 450, 60, 80} {
		word := fmt.Sprintf("w%02d", i)
		counts[word] = count
		words = append(words, word)
	}
	counts["absent"] = 0
	counts["huge"] = 2000
	index := &fakeIndex{chunks: 10, counts: counts, hits: []Hit{{ChunkID: "c1", Collection: "support"}}}
	retriever, _ := newRetriever(t, index, nil)

	hits, err := retriever.Search(t.Context(), []string{"support"}, strings.Join(append(words, "absent", "huge"), " "), SearchDepth)
	if err != nil || len(hits) != 1 {
		t.Fatalf("Search = %v, %v", hits, err)
	}
	want := []string{"w07", "w05", "w06", "w03", "w01", "w02", "w09", "w10"}
	if len(index.searchTerms) != 1 || !reflect.DeepEqual(index.searchTerms[0], want) {
		t.Errorf("final terms = %v, want %v", index.searchTerms, want)
	}
	for _, limit := range index.limits {
		if limit != MinTermCap+1 {
			t.Errorf("count limit = %d, want %d", limit, MinTermCap+1)
		}
	}
	if len(index.counted) != len(words)+2 {
		t.Errorf("counted %d terms, want %d", len(index.counted), len(words)+2)
	}
}

func TestSearchDropsTermsAboveTenPercentOfALargeScope(t *testing.T) {
	index := &fakeIndex{chunks: 500000, counts: map[string]int{"common": 60000, "rare": 20000, "mid": 15000}, hits: []Hit{{ChunkID: "c1", Collection: "support"}}}
	retriever, _ := newRetriever(t, index, nil)
	if _, err := retriever.Search(t.Context(), []string{"support"}, "common rare mid", SearchDepth); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if want := []string{"mid"}; !reflect.DeepEqual(index.searchTerms[0], want) {
		t.Errorf("final terms = %v, want %v (rare would push the count sum past the budget)", index.searchTerms[0], want)
	}
	for _, limit := range index.limits {
		if limit != 50001 {
			t.Errorf("count limit = %d, want threshold+1 = 50001", limit)
		}
	}
}

func TestSearchKeepsTheSingleRarestTermEvenWhenItAloneExceedsTheCountSum(t *testing.T) {
	index := &fakeIndex{chunks: 1000000, counts: map[string]int{"alpha": 40000, "beta": 45000}, hits: []Hit{{ChunkID: "c1", Collection: "support"}}}
	retriever, _ := newRetriever(t, index, nil)
	if _, err := retriever.Search(t.Context(), []string{"support"}, "beta alpha", SearchDepth); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if want := []string{"alpha"}; !reflect.DeepEqual(index.searchTerms[0], want) {
		t.Errorf("final terms = %v, want %v", index.searchTerms[0], want)
	}
}

func TestSearchBreaksCountTiesByTerm(t *testing.T) {
	index := &fakeIndex{chunks: 10, counts: map[string]int{"delta": 4, "alpha": 4, "charlie": 4, "bravo": 4}, hits: []Hit{{ChunkID: "c1", Collection: "support"}}}
	retriever, _ := newRetriever(t, index, nil)
	if _, err := retriever.Search(t.Context(), []string{"support"}, "delta charlie bravo alpha", SearchDepth); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if want := []string{"alpha", "bravo", "charlie", "delta"}; !reflect.DeepEqual(index.searchTerms[0], want) {
		t.Errorf("final terms = %v, want %v", index.searchTerms[0], want)
	}
}

func TestSearchWithNoUsableTermsIsEmptyNotAnError(t *testing.T) {
	tests := map[string]struct {
		query  string
		counts map[string]int
	}{
		"blank":      {query: "   "},
		"punctuated": {query: "?!"},
		"all absent": {query: "ghost spectre", counts: map[string]int{}},
		"all capped": {query: "the of", counts: map[string]int{"the": 5000, "of": 5000}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			index := &fakeIndex{chunks: 10, counts: test.counts, hits: []Hit{{ChunkID: "c1", Collection: "support"}}}
			retriever, secrets := newRetriever(t, index, nil)
			hits, err := retriever.Search(t.Context(), []string{"support"}, test.query, SearchDepth)
			if err != nil || len(hits) != 0 {
				t.Fatalf("Search = %v, %v", hits, err)
			}
			if len(index.searchTerms) != 0 || secrets.calls != 0 {
				t.Errorf("searched %v with %d secret snapshots", index.searchTerms, secrets.calls)
			}
		})
	}
}

func TestSearchPassesTheWholeScopeAndRejectsBadScopes(t *testing.T) {
	index := &fakeIndex{chunks: 10, counts: map[string]int{"refund": 1}, hits: []Hit{{ChunkID: "c1", Collection: "support"}}}
	retriever, _ := newRetriever(t, index, nil)
	if _, err := retriever.Search(t.Context(), []string{"support", "hr-docs", "support"}, "refund", SearchDepth); err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, scope := range index.scopes {
		if want := []string{"support", "hr-docs"}; !reflect.DeepEqual(scope, want) {
			t.Errorf("index scope = %v, want %v", scope, want)
		}
	}
	for name, scope := range map[string][]string{"none": nil, "invalid": {"Bad Id"}, "empty": {""}} {
		t.Run(name, func(t *testing.T) {
			_, err := retriever.Search(t.Context(), scope, "refund", SearchDepth)
			if code, ok := CodeOf(err); !ok || (code != ErrorCodeInvalidArgument && code != ErrorCodeCollectionInvalid) {
				t.Errorf("error = %v", err)
			}
		})
	}
	for _, depth := range []int{0, -1, SearchDepth + 1} {
		if _, err := retriever.Search(t.Context(), []string{"support"}, "refund", depth); err == nil {
			t.Errorf("depth %d accepted", depth)
		}
	}
}

func TestSearchDropsHitsOutsideTheScopeAndVaultMatches(t *testing.T) {
	index := &fakeIndex{chunks: 10, counts: map[string]int{"refund": 3}, hits: []Hit{
		{ChunkID: "kept", Collection: "support", Content: "refund policy"},
		{ChunkID: "stray", Collection: "other", Content: "refund policy"},
		{ChunkID: "content", Collection: "support", Content: "refund token vault-secret-value"},
		{ChunkID: "heading", Collection: "support", Heading: "vault-secret-value", Content: "refund"},
		{ChunkID: "label", Collection: "support", Label: "vault-secret-value", Content: "refund"},
	}}
	retriever, _ := newRetriever(t, index, fakeVault{values: []string{"vault-secret-value"}})
	hits, err := retriever.Search(t.Context(), []string{"support"}, "refund", SearchDepth)
	if err != nil || len(hits) != 1 || hits[0].ChunkID != "kept" {
		t.Fatalf("hits = %+v, %v", hits, err)
	}
}

func TestSearchFailsClosedWhenTheVaultCannotBeResolved(t *testing.T) {
	index := &fakeIndex{chunks: 10, counts: map[string]int{"refund": 1}, hits: []Hit{{ChunkID: "c1", Collection: "support", Content: "refund"}}}
	secrets := &fakeSecrets{err: errors.New("env missing")}
	retriever, err := NewRetriever(index, secrets)
	if err != nil {
		t.Fatalf("NewRetriever: %v", err)
	}
	hits, err := retriever.Search(t.Context(), []string{"support"}, "refund", SearchDepth)
	if code, ok := CodeOf(err); !ok || code != ErrorCodeSecretUnavailable || len(hits) != 0 {
		t.Fatalf("hits = %v, error = %v", hits, err)
	}
}

func TestSearchSurfacesIndexErrors(t *testing.T) {
	boom := errors.New("index down")
	retriever, _ := newRetriever(t, &fakeIndex{err: boom}, nil)
	if _, err := retriever.Search(t.Context(), []string{"support"}, "refund", SearchDepth); !errors.Is(err, boom) {
		t.Errorf("error = %v, want index error", err)
	}
}

func TestNewRetrieverRequiresItsDependencies(t *testing.T) {
	if _, err := NewRetriever(nil, &fakeSecrets{}); err == nil {
		t.Error("nil index accepted")
	}
	if _, err := NewRetriever(&fakeIndex{}, nil); err == nil {
		t.Error("nil secrets accepted")
	}
}
