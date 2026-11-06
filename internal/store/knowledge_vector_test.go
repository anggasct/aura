package store

import (
	"database/sql"
	"math"
	"slices"
	"strings"
	"testing"
)

func embedCollection(t *testing.T, embeddings KBEmbeddingStore, collection, fingerprint string, dimensions int) []KBPendingChunk {
	t.Helper()
	if err := embeddings.StartEmbedding(t.Context(), collection, fingerprint); err != nil {
		t.Fatalf("StartEmbedding: %v", err)
	}
	pending := pendingChunks(t, embeddings, collection, fingerprint)
	if _, err := embeddings.CommitVectors(t.Context(), collection, fingerprint, vectorWrites(pending, dimensions)); err != nil {
		t.Fatalf("CommitVectors: %v", err)
	}
	return pending
}

func TestKBVectorReaderReportsGenerationAndFingerprintPerCollection(t *testing.T) {
	db, kb, embeddings := newEmbeddingStore(t)
	reader := NewKBVectorReader(db)
	writeKBSource(t, kb, "support", "/docs/a.md", "alpha text")
	writeKBSource(t, kb, "faq", "/docs/b.md", "beta text")
	embedCollection(t, embeddings, "support", embedFingerprintA, 4)

	versions, err := reader.CollectionVersions(t.Context(), []string{"support", "faq", "missing"})
	if err != nil {
		t.Fatalf("CollectionVersions: %v", err)
	}
	byCollection := map[string]KBCollectionVersion{}
	for _, version := range versions {
		byCollection[version.Collection] = version
	}
	if len(versions) != 2 || byCollection["support"].Fingerprint != embedFingerprintA || byCollection["faq"].Fingerprint != "" {
		t.Fatalf("versions = %+v", versions)
	}
	if want := kbGeneration(t, db, "support"); byCollection["support"].Generation != want {
		t.Errorf("support generation = %d, want %d", byCollection["support"].Generation, want)
	}

	before := byCollection["support"].Generation
	if _, err := embeddings.DisableEmbedding(t.Context(), "support", true); err != nil {
		t.Fatalf("DisableEmbedding: %v", err)
	}
	after, err := reader.CollectionVersions(t.Context(), []string{"support"})
	if err != nil || len(after) != 1 || after[0].Generation <= before || after[0].Fingerprint != "" {
		t.Fatalf("versions after --off = %+v, %v; want a newer generation and no fingerprint", after, err)
	}
}

func TestKBVectorReaderRejectsAnInvalidScope(t *testing.T) {
	reader := NewKBVectorReader(newTestDB(t))
	if _, err := reader.CollectionVersions(t.Context(), nil); err == nil {
		t.Error("empty scope accepted")
	}
	if _, err := reader.CollectionVersions(t.Context(), []string{""}); err == nil {
		t.Error("empty collection accepted")
	}
	if _, err := NewKBVectorReader(nil).CollectionVersions(t.Context(), []string{"a"}); err == nil {
		t.Error("nil database accepted")
	}
}

func TestKBVectorReaderPagesVectorsOfOneFingerprintOfOneCollection(t *testing.T) {
	db, kb, embeddings := newEmbeddingStore(t)
	reader := NewKBVectorReader(db)
	writeKBSource(t, kb, "support", "/docs/a.md", "alpha", "beta", "gamma", "delta", "epsilon")
	writeKBSource(t, kb, "faq", "/docs/b.md", "zeta")
	support := embedCollection(t, embeddings, "support", embedFingerprintA, 4)
	embedCollection(t, embeddings, "faq", embedFingerprintA, 4)

	var rowIDs []int64
	var values []float32
	var after int64
	for {
		page, err := reader.VectorPage(t.Context(), "support", embedFingerprintA, 4, after, 2)
		if err != nil {
			t.Fatalf("VectorPage: %v", err)
		}
		if len(page.RowIDs) == 0 {
			break
		}
		if len(page.Digests) != len(page.RowIDs) || len(page.Values) != 4*len(page.RowIDs) {
			t.Fatalf("page shape = %d rows, %d digests, %d values", len(page.RowIDs), len(page.Digests), len(page.Values))
		}
		rowIDs = append(rowIDs, page.RowIDs...)
		values = append(values, page.Values...)
		after = page.RowIDs[len(page.RowIDs)-1]
	}
	if len(rowIDs) != 5 || !slices.IsSorted(rowIDs) {
		t.Fatalf("rows = %v, want the five support chunks in order", rowIDs)
	}
	for i, chunk := range support {
		want := unitVector(4, float64(chunk.RowID))
		for j := range want {
			if math.Abs(float64(values[i*4+j]-want[j])) > 1e-6 {
				t.Fatalf("vector %d differs at %d: %v vs %v", i, j, values[i*4:(i+1)*4], want)
			}
		}
	}

	other, err := reader.VectorPage(t.Context(), "support", embedFingerprintB, 4, 0, 10)
	if err != nil || len(other.RowIDs) != 0 {
		t.Fatalf("another fingerprint returned %d rows, %v", len(other.RowIDs), err)
	}
}

func TestKBVectorReaderSkipsVectorsWhoseChunkChanged(t *testing.T) {
	db, kb, embeddings := newEmbeddingStore(t)
	reader := NewKBVectorReader(db)
	writeKBSource(t, kb, "support", "/docs/a.md", "alpha", "beta")
	embedCollection(t, embeddings, "support", embedFingerprintA, 4)
	if _, err := db.ExecContext(t.Context(), `UPDATE kb_chunk SET content_sha256 = ? WHERE ordinal = 0`, "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"); err != nil {
		t.Fatalf("rewrite digest: %v", err)
	}
	page, err := reader.VectorPage(t.Context(), "support", embedFingerprintA, 4, 0, 10)
	if err != nil || len(page.RowIDs) != 1 {
		t.Fatalf("page = %d rows, %v, want only the unchanged chunk", len(page.RowIDs), err)
	}
}

func TestKBVectorReaderRejectsAMisshapenRequestAndABlobOfAnotherWidth(t *testing.T) {
	db, kb, embeddings := newEmbeddingStore(t)
	reader := NewKBVectorReader(db)
	writeKBSource(t, kb, "support", "/docs/a.md", "alpha")
	embedCollection(t, embeddings, "support", embedFingerprintA, 4)

	tests := map[string]struct {
		collection, fingerprint string
		dimensions, limit       int
	}{
		"no collection":   {fingerprint: embedFingerprintA, dimensions: 4, limit: 1},
		"no fingerprint":  {collection: "support", dimensions: 4, limit: 1},
		"no dimensions":   {collection: "support", fingerprint: embedFingerprintA, limit: 1},
		"no limit":        {collection: "support", fingerprint: embedFingerprintA, dimensions: 4},
		"limit too large": {collection: "support", fingerprint: embedFingerprintA, dimensions: 4, limit: KBMaxVectorPage + 1},
		"wrong width":     {collection: "support", fingerprint: embedFingerprintA, dimensions: 8, limit: 1},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := reader.VectorPage(t.Context(), test.collection, test.fingerprint, test.dimensions, 0, test.limit); err == nil {
				t.Error("request accepted")
			}
		})
	}
}

func TestKBVectorReaderReadsChunksByRowIDWithTheirDigest(t *testing.T) {
	db, kb, _ := newEmbeddingStore(t)
	reader := NewKBVectorReader(db)
	writeKBSource(t, kb, "support", "/docs/a.md", "alpha", "beta")
	writeKBSource(t, kb, "faq", "/docs/b.md", "gamma")
	rowIDs := chunkRowIDs(t, db)

	chunks, err := reader.ChunksByRowID(t.Context(), append(slices.Clone(rowIDs), 99999))
	if err != nil || len(chunks) != 3 {
		t.Fatalf("chunks = %d, %v, want the three that exist", len(chunks), err)
	}
	byRow := map[int64]KBVectorChunk{}
	for _, chunk := range chunks {
		byRow[chunk.RowID] = chunk
	}
	first := byRow[rowIDs[0]]
	if first.Collection != "support" || first.Content != "alpha" || first.SHA256 != kbDigest("alpha") || first.Label != "notes.md" || first.Heading != "Section" || first.ChunkID == "" {
		t.Errorf("first = %+v", first)
	}
	if byRow[rowIDs[2]].Collection != "faq" {
		t.Errorf("third = %+v, want the faq chunk", byRow[rowIDs[2]])
	}

	if _, err := reader.ChunksByRowID(t.Context(), nil); err == nil {
		t.Error("an empty lookup was accepted")
	}
	if _, err := reader.ChunksByRowID(t.Context(), make([]int64, KBMaxVectorChunks+1)); err == nil {
		t.Error("an oversized lookup was accepted")
	}
}

func TestKBVectorPageQueryWalksTheVectorKeyWithoutASort(t *testing.T) {
	db, kb, embeddings := newEmbeddingStore(t)
	writeKBSource(t, kb, "support", "/docs/a.md", "alpha", "beta")
	embedCollection(t, embeddings, "support", embedFingerprintA, 4)
	plan := queryPlan(t, db, kbVectorPageSQL, 0, "support", embedFingerprintA, 10)
	if len(plan) == 0 || !strings.Contains(plan[0], "SEARCH e USING INTEGER PRIMARY KEY") || strings.Contains(strings.Join(plan, "\n"), "TEMP B-TREE") {
		t.Fatalf("plan = %q, want a walk of the vector table by key without a sort", plan)
	}
}

func chunkRowIDs(t *testing.T, db *sql.DB) []int64 {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT rowid FROM kb_chunk ORDER BY rowid`)
	if err != nil {
		t.Fatalf("list rows: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var rowIDs []int64
	for rows.Next() {
		var rowID int64
		if err := rows.Scan(&rowID); err != nil {
			t.Fatalf("scan: %v", err)
		}
		rowIDs = append(rowIDs, rowID)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list rows: %v", err)
	}
	return rowIDs
}

func queryPlan(t *testing.T, db *sql.DB, statement string, args ...any) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `EXPLAIN QUERY PLAN `+statement, args...)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("scan: %v", err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	return plan
}
