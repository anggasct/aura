package cli

import (
	"context"
	"database/sql"
	"encoding/binary"
	"math"
	"math/rand"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/store/storetest"
)

const (
	benchVectorChunks      = 100000
	benchVectorDimensions  = 1024
	benchVectorFingerprint = "bench-fingerprint"
	benchVectorMemory      = 512 << 20
)

type benchEmbedder struct {
	vector []float32
}

func (e benchEmbedder) Embed(context.Context, knowledge.EmbedRole, []string) ([][]float32, error) {
	return [][]float32{e.vector}, nil
}

func benchUnitVector(random *rand.Rand, dimensions int) []float32 {
	vector := make([]float32, dimensions)
	var sum float64
	for i := range vector {
		vector[i] = random.Float32() - 0.5
		sum += float64(vector[i]) * float64(vector[i])
	}
	norm := float32(math.Sqrt(sum))
	for i := range vector {
		vector[i] /= norm
	}
	return vector
}

func seedBenchVectors(b *testing.B, db *sql.DB, collection string) {
	b.Helper()
	ctx := b.Context()
	if _, err := db.ExecContext(ctx, `UPDATE kb_collection SET embedding_fingerprint = ? WHERE id = ?`, benchVectorFingerprint, collection); err != nil {
		b.Fatalf("opt in: %v", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		b.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	statement, err := tx.PrepareContext(ctx, `INSERT INTO kb_embedding (chunk_rowid, fingerprint, content_sha256, vector) SELECT rowid, ?, content_sha256, ? FROM kb_chunk WHERE rowid = ?`)
	if err != nil {
		b.Fatalf("prepare: %v", err)
	}
	defer func() { _ = statement.Close() }()
	rowIDs := benchChunkRowIDs(b, tx)
	random := rand.New(rand.NewSource(11))
	blob := make([]byte, benchVectorDimensions*4)
	for _, rowID := range rowIDs {
		for i, value := range benchUnitVector(random, benchVectorDimensions) {
			binary.LittleEndian.PutUint32(blob[i*4:], math.Float32bits(value))
		}
		if _, err := statement.ExecContext(ctx, benchVectorFingerprint, blob, rowID); err != nil {
			b.Fatalf("insert vector: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatalf("commit: %v", err)
	}
}

func BenchmarkKnowledgeHybridSearchOverAHundredThousandVectors(b *testing.B) {
	path := filepath.Join(b.TempDir(), "aura.db")
	storetest.Seed(b, path)
	db, err := store.OpenDB(b.Context(), path)
	if err != nil {
		b.Fatalf("OpenDB: %v", err)
	}
	b.Cleanup(func() { _ = db.Close() })
	seedBulkKBScope(b, db, "bench", benchVectorChunks, `'passage ' || i || ' about topic ' || (i % 50) || CASE WHEN i % 1000 = 0 THEN ' rare' ELSE '' END`)
	seedBenchVectors(b, db, "bench")

	pool, err := openKnowledgeReadPool(b.Context(), path)
	if err != nil {
		b.Fatalf("openKnowledgeReadPool: %v", err)
	}
	b.Cleanup(func() { _ = pool.Close() })
	loader, err := store.OpenReadOnly(b.Context(), path)
	if err != nil {
		b.Fatalf("OpenReadOnly: %v", err)
	}
	loader.SetMaxOpenConns(knowledgeLoaderConns)
	b.Cleanup(func() { _ = loader.Close() })

	index, err := knowledge.NewMemoryIndex(&knowledge.MemoryIndexConfig{
		Reader: newKnowledgeVectorSource(pool), Loader: newKnowledgeVectorSource(loader),
		Fingerprint: benchVectorFingerprint, Dimensions: benchVectorDimensions, MaxMemory: benchVectorMemory,
	})
	if err != nil {
		b.Fatalf("NewMemoryIndex: %v", err)
	}
	runCtx, cancel := context.WithCancel(b.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		index.Run(runCtx)
	}()
	b.Cleanup(func() {
		cancel()
		<-done
	})
	settings := &knowledge.EmbeddingSettings{
		Fingerprint: benchVectorFingerprint, Destination: "bench.invalid", Model: "bench", Dimensions: benchVectorDimensions,
		BatchSize: 96, MaxMemory: benchVectorMemory,
	}
	embedder := benchEmbedder{vector: benchUnitVector(rand.New(rand.NewSource(99)), benchVectorDimensions)}
	retriever, err := knowledge.NewRetriever(&knowledgeIndex{index: store.NewKBIndex(pool)}, benchSecrets{},
		knowledge.WithVectors(&knowledge.VectorConfig{Index: index, Embedder: embedder, Settings: settings}))
	if err != nil {
		b.Fatalf("NewRetriever: %v", err)
	}

	request := &knowledge.SearchRequest{Scope: []string{"bench"}, Query: "which passage about topic 7 is rare", Depth: knowledge.SearchDepth, Class: principal.ClassOwner}
	loadStarted := time.Now()
	if _, err := retriever.Search(b.Context(), request); err != nil {
		b.Fatalf("cold Search: %v", err)
	}
	idleCtx, idleCancel := context.WithTimeout(b.Context(), 5*time.Minute)
	if err := index.Idle(idleCtx); err != nil {
		b.Fatalf("load never finished: %v", err)
	}
	idleCancel()
	loadTook := time.Since(loadStarted)
	if index.Used() == 0 {
		b.Fatal("the collection did not load: raise the memory limit")
	}
	runtime.GC()
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)

	var elapsed []time.Duration
	b.ResetTimer()
	for b.Loop() {
		started := time.Now()
		found, err := retriever.Search(b.Context(), request)
		took := time.Since(started)
		if err != nil || found.Mode != knowledge.ModeHybrid || len(found.Hits) == 0 {
			b.Fatalf("Search = mode %q, %d hits, %v", found.Mode, len(found.Hits), err)
		}
		elapsed = append(elapsed, took)
	}
	b.StopTimer()
	slices.Sort(elapsed)
	worst := elapsed[len(elapsed)-1]
	b.ReportMetric(float64(loadTook.Milliseconds()), "load-ms")
	b.ReportMetric(float64(memory.HeapAlloc)/(1<<20), "heap-mib")
	b.ReportMetric(float64(elapsed[len(elapsed)/2].Milliseconds()), "p50-ms")
	b.ReportMetric(float64(worst.Milliseconds()), "worst-ms")
	if worst > benchBudget {
		b.Fatalf("worst search took %s, budget %s", worst, benchBudget)
	}
}

func benchChunkRowIDs(b *testing.B, tx *sql.Tx) []int64 {
	b.Helper()
	rows, err := tx.QueryContext(b.Context(), `SELECT rowid FROM kb_chunk ORDER BY rowid`)
	if err != nil {
		b.Fatalf("list chunks: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var rowIDs []int64
	for rows.Next() {
		var rowID int64
		if err := rows.Scan(&rowID); err != nil {
			b.Fatalf("scan: %v", err)
		}
		rowIDs = append(rowIDs, rowID)
	}
	if err := rows.Err(); err != nil {
		b.Fatalf("list chunks: %v", err)
	}
	return rowIDs
}
