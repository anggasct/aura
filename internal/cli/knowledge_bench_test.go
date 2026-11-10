package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/store/storetest"
)

const (
	benchTargetChunks = 200000
	benchNoiseChunks  = 300000
	benchTermCount    = 16
	benchBudget       = 500 * time.Millisecond
)

type benchScreen struct{}

func (benchScreen) Contains(string) bool { return false }

type benchSecrets struct{}

func (benchSecrets) Snapshot(context.Context) (knowledge.SecretScreen, error) {
	return benchScreen{}, nil
}

func benchTargetContent() string {
	parts := []string{`'w' || (i % 997) || ' filler'`,
		`CASE WHEN i % 100 < 7 THEN ' t01' ELSE '' END`,
		`CASE WHEN i % 100 >= 50 AND i % 100 < 57 THEN ' t02' ELSE '' END`}
	for term := 3; term <= benchTermCount; term++ {
		parts = append(parts, fmt.Sprintf(`CASE WHEN (i + %d) %% 200 < 19 THEN ' t%02d' ELSE '' END`, term*7, term))
	}
	return strings.Join(parts, " || ")
}

func benchNoiseContent() string {
	var terms []string
	for term := 1; term <= benchTermCount; term++ {
		terms = append(terms, fmt.Sprintf("t%02d", term))
	}
	return `'w' || (i % 997) || CASE WHEN i % 3 = 0 THEN ' ` + strings.Join(terms, " ") + `' ELSE ' filler' END`
}

func BenchmarkKnowledgeSearchLargeScope(b *testing.B) {
	path := filepath.Join(b.TempDir(), "aura.db")
	storetest.Seed(b, path)
	db, err := store.OpenDB(b.Context(), path)
	if err != nil {
		b.Fatalf("OpenDB: %v", err)
	}
	b.Cleanup(func() { _ = db.Close() })
	seedBulkKBScope(b, db, "bench", benchTargetChunks, benchTargetContent())
	seedBulkKBScope(b, db, "noise", benchNoiseChunks, benchNoiseContent())
	pool, err := openKnowledgeReadPool(b.Context(), path)
	if err != nil {
		b.Fatalf("openKnowledgeReadPool: %v", err)
	}
	b.Cleanup(func() { _ = pool.Close() })
	retriever, err := knowledge.NewRetriever(&knowledgeIndex{index: store.NewKBIndex(pool)}, benchSecrets{})
	if err != nil {
		b.Fatalf("NewRetriever: %v", err)
	}
	var terms []string
	for term := 1; term <= benchTermCount; term++ {
		terms = append(terms, fmt.Sprintf("t%02d", term))
	}
	query := strings.Join(terms, " ")
	if _, err := ownerSearch(b.Context(), retriever, []string{"bench"}, query, knowledge.SearchDepth); err != nil {
		b.Fatalf("warm-up Search: %v", err)
	}

	var elapsed []time.Duration
	b.ResetTimer()
	for b.Loop() {
		started := time.Now()
		hits, err := ownerSearch(b.Context(), retriever, []string{"bench"}, query, knowledge.SearchDepth)
		took := time.Since(started)
		if err != nil || len(hits) == 0 {
			b.Fatalf("Search = %d hits, %v", len(hits), err)
		}
		elapsed = append(elapsed, took)
	}
	b.StopTimer()
	slices.Sort(elapsed)
	worst := elapsed[len(elapsed)-1]
	b.ReportMetric(float64(elapsed[len(elapsed)/2].Milliseconds()), "p50-ms")
	b.ReportMetric(float64(worst.Milliseconds()), "worst-ms")
	if worst > benchBudget {
		b.Fatalf("worst search took %s, budget %s", worst, benchBudget)
	}
}
