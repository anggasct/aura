package store

import (
	"fmt"
	"strings"
	"testing"
)

func kbSeed(t *testing.T, kb KBStore, collection, uri string, texts ...string) {
	t.Helper()
	if _, err := kb.ReplaceSource(t.Context(), kbWrite(collection, uri, "a", kbChunks(texts...))); err != nil {
		t.Fatalf("seed %s: %v", collection, err)
	}
}

func kbHitCollections(hits []KBHit) []string {
	seen := map[string]bool{}
	var out []string
	for i := range hits {
		if !seen[hits[i].Collection] {
			seen[hits[i].Collection] = true
			out = append(out, hits[i].Collection)
		}
	}
	return out
}

func TestKBIndexSearchStemsTermsAndStaysInsideTheScope(t *testing.T) {
	db := newTestDB(t)
	kb := NewKBStore(db)
	kbSeed(t, kb, "support", "/docs/a.md", "Customers may request a Refund within thirty days.", "Shipping takes five days.")
	kbSeed(t, kb, "hr-docs", "/docs/b.md", "Employees file a refund claim with finance.")
	kbSeed(t, kb, "hrdocs", "/docs/c.md", "The refund desk is closed on Sundays.")
	index := NewKBIndex(db)

	hits, err := index.Search(t.Context(), []string{"support"}, []string{"refunds"}, 50)
	if err != nil || len(hits) != 1 || hits[0].Collection != "support" || !strings.Contains(hits[0].Content, "Refund") {
		t.Fatalf("support hits = %+v, %v", hits, err)
	}
	if hits[0].Label != "notes.md" || hits[0].Heading != "Section" || hits[0].ChunkID == "" || hits[0].SourceID == "" {
		t.Errorf("hit metadata = %+v", hits[0])
	}

	for name, scope := range map[string][]string{
		"hyphen":       {"hr-docs"},
		"no hyphen":    {"hrdocs"},
		"both":         {"hr-docs", "hrdocs"},
		"unknown":      {"hr-doc"},
		"support only": {"support"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := index.Search(t.Context(), scope, []string{"refund"}, 50)
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			for _, collection := range kbHitCollections(got) {
				listed := false
				for _, want := range scope {
					listed = listed || want == collection
				}
				if !listed {
					t.Errorf("scope %v returned a hit from %q", scope, collection)
				}
			}
		})
	}
}

func TestKBIndexTermsCannotChangeTheQueryOrTheScope(t *testing.T) {
	db := newTestDB(t)
	kb := NewKBStore(db)
	kbSeed(t, kb, "support", "/docs/a.md", "Refund policy for customers.")
	kbSeed(t, kb, "secret", "/docs/b.md", "Confidential merger plans.")
	index := NewKBIndex(db)

	hostile := []string{
		`"`, `""`, `refund" OR "merger`, `scope:` + KBScope("secret"), KBScope("secret"),
		`NEAR(refund merger)`, `* OR *`, `merger*`, `-merger`, `refund AND NOT policy`, `{content}:merger`, `^merger`, `)`, `(`,
	}
	for _, term := range hostile {
		t.Run(term, func(t *testing.T) {
			hits, err := index.Search(t.Context(), []string{"support"}, []string{term}, 50)
			if err != nil {
				t.Fatalf("Search(%q): %v", term, err)
			}
			for _, collection := range kbHitCollections(hits) {
				if collection != "support" {
					t.Errorf("term %q reached collection %q", term, collection)
				}
			}
			for _, hit := range hits {
				if strings.Contains(hit.Content, "merger") {
					t.Errorf("term %q reached other content: %q", term, hit.Content)
				}
			}
			if count, err := index.TermFrequency(t.Context(), []string{"support"}, term, 10); err != nil || count > 1 {
				t.Errorf("TermFrequency(%q) = %d, %v", term, count, err)
			}
		})
	}
}

func TestKBIndexScopeTokenIsNotSearchableAsContent(t *testing.T) {
	db := newTestDB(t)
	kb := NewKBStore(db)
	kbSeed(t, kb, "support", "/docs/a.md", "Refund policy.")
	kbSeed(t, kb, "other", "/docs/b.md", "Shipping policy.")
	index := NewKBIndex(db)

	for _, term := range []string{KBScope("other"), KBScope("support")} {
		count, err := index.TermFrequency(t.Context(), []string{"support"}, term, 10)
		if err != nil || count != 0 {
			t.Errorf("TermFrequency(scope token) = %d, %v; want 0", count, err)
		}
		hits, err := index.Search(t.Context(), []string{"support"}, []string{term}, 50)
		if err != nil || len(hits) != 0 {
			t.Errorf("Search(scope token) = %+v, %v; want none", hits, err)
		}
	}
}

func TestKBIndexTermFrequencyStopsAtTheLimit(t *testing.T) {
	db := newTestDB(t)
	kb := NewKBStore(db)
	texts := make([]string, 0, 12)
	for i := range 12 {
		texts = append(texts, fmt.Sprintf("common word number %d", i))
	}
	kbSeed(t, kb, "support", "/docs/a.md", texts...)
	index := NewKBIndex(db)

	for limit, want := range map[int]int{3: 3, 12: 12, 50: 12} {
		got, err := index.TermFrequency(t.Context(), []string{"support"}, "common", limit)
		if err != nil || got != want {
			t.Errorf("TermFrequency limit %d = %d, %v; want %d", limit, got, err, want)
		}
	}
	if got, err := index.TermFrequency(t.Context(), []string{"support"}, "absent", 10); err != nil || got != 0 {
		t.Errorf("absent term = %d, %v", got, err)
	}
}

func TestKBIndexRanksRareMatchesFirstAndHonoursDepth(t *testing.T) {
	db := newTestDB(t)
	kb := NewKBStore(db)
	texts := []string{"refund refund refund policy for customers", "policy overview", "policy details", "policy notes", "refund note"}
	kbSeed(t, kb, "support", "/docs/a.md", texts...)
	index := NewKBIndex(db)

	hits, err := index.Search(t.Context(), []string{"support"}, []string{"refund", "policy"}, 50)
	if err != nil || len(hits) != 5 {
		t.Fatalf("hits = %d, %v", len(hits), err)
	}
	if hits[0].Content != texts[0] {
		t.Errorf("top hit = %q, want %q", hits[0].Content, texts[0])
	}
	for i := 1; i < len(hits); i++ {
		if hits[i].Score < hits[i-1].Score {
			t.Errorf("hits are not ordered by score at %d", i)
		}
	}
	limited, err := index.Search(t.Context(), []string{"support"}, []string{"refund", "policy"}, 2)
	if err != nil || len(limited) != 2 || limited[0].ChunkID != hits[0].ChunkID {
		t.Errorf("limited = %+v, %v", limited, err)
	}
}

func TestKBIndexCountsScopesAndSizes(t *testing.T) {
	db := newTestDB(t)
	kb := NewKBStore(db)
	kbSeed(t, kb, "support", "/docs/a.md", "one", "two", "three")
	kbSeed(t, kb, "other", "/docs/b.md", "four")
	index := NewKBIndex(db)

	tests := map[string]struct {
		scope []string
		want  int
	}{
		"support": {scope: []string{"support"}, want: 3},
		"other":   {scope: []string{"other"}, want: 1},
		"both":    {scope: []string{"support", "other"}, want: 4},
		"unknown": {scope: []string{"missing"}, want: 0},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := index.ScopeChunks(t.Context(), test.scope)
			if err != nil || got != test.want {
				t.Errorf("ScopeChunks(%v) = %d, %v; want %d", test.scope, got, err, test.want)
			}
		})
	}
}

func TestKBIndexRejectsInvalidInput(t *testing.T) {
	db := newTestDB(t)
	index := NewKBIndex(db)
	tooMany := make([]string, KBMaxSearchCollections+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("c%d", i)
	}
	terms := make([]string, KBMaxSearchTerms+1)
	for i := range terms {
		terms[i] = fmt.Sprintf("t%d", i)
	}
	tests := map[string]func() error{
		"no collections":      func() error { _, err := index.Search(t.Context(), nil, []string{"a"}, 10); return err },
		"empty collection":    func() error { _, err := index.Search(t.Context(), []string{""}, []string{"a"}, 10); return err },
		"too many collection": func() error { _, err := index.Search(t.Context(), tooMany, []string{"a"}, 10); return err },
		"no terms":            func() error { _, err := index.Search(t.Context(), []string{"a"}, nil, 10); return err },
		"too many terms":      func() error { _, err := index.Search(t.Context(), []string{"a"}, terms, 10); return err },
		"zero depth":          func() error { _, err := index.Search(t.Context(), []string{"a"}, []string{"a"}, 0); return err },
		"deep": func() error {
			_, err := index.Search(t.Context(), []string{"a"}, []string{"a"}, KBMaxSearchDepth+1)
			return err
		},
		"empty term count": func() error { _, err := index.TermFrequency(t.Context(), []string{"a"}, "", 5); return err },
		"zero limit":       func() error { _, err := index.TermFrequency(t.Context(), []string{"a"}, "a", 0); return err },
		"count no scope":   func() error { _, err := index.ScopeChunks(t.Context(), nil); return err },
	}
	for name, call := range tests {
		t.Run(name, func(t *testing.T) {
			err := call()
			code, ok := CodeOf(err)
			if !ok || code != ErrorCodeInvalidArgument {
				t.Errorf("error = %v, want invalid_argument", err)
			}
		})
	}
	nilIndex := &sqliteKBIndex{}
	if _, err := nilIndex.ScopeChunks(t.Context(), []string{"a"}); err == nil {
		t.Error("nil db accepted")
	}
}

func TestKBIndexPresentReportsAPreKnowledgeDatabase(t *testing.T) {
	db := newTestDB(t)
	present, err := KBIndexPresent(t.Context(), db)
	if err != nil || !present {
		t.Fatalf("migrated database: present = %t, %v", present, err)
	}
	for _, drop := range []string{"DROP TABLE kb_chunk_fts", "DROP TABLE kb_chunk", "DROP TABLE kb_source", "DROP TABLE kb_collection"} {
		if _, err := db.ExecContext(t.Context(), drop); err != nil {
			t.Fatalf("%s: %v", drop, err)
		}
		if present, err := KBIndexPresent(t.Context(), db); err != nil || present {
			t.Fatalf("after %q: present = %t, %v", drop, present, err)
		}
	}
	if _, err := KBIndexPresent(t.Context(), nil); err == nil {
		t.Error("nil database accepted")
	}
}
