package knowledge

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/sqlite"
)

const (
	embedFingerprintA = "fingerprint-a"
	embedFingerprintB = "fingerprint-b"
)

func unitVector(dimensions int, seed float64) []float32 {
	vector := make([]float32, dimensions)
	var sum float64
	for i := range vector {
		value := math.Sin(float64(i)*0.37 + seed)
		vector[i] = float32(value)
		sum += float64(vector[i]) * float64(vector[i])
	}
	norm := float32(math.Sqrt(sum))
	for i := range vector {
		vector[i] /= norm
	}
	return vector
}

func newEmbeddingStore(t *testing.T) (*sql.DB, *SQLiteStore) {
	t.Helper()
	db := newKBTestDB(t)
	return db, mustNewSQLiteStore(t, db)
}

func writeKBSource(t *testing.T, kb Store, collection, uri string, texts ...string) {
	t.Helper()
	if _, err := kb.ReplaceSource(t.Context(), kbWrite(collection, uri, "a", kbChunks(texts...))); err != nil {
		t.Fatalf("ReplaceSource(%s): %v", uri, err)
	}
}

func pendingChunks(t *testing.T, embeddings EmbeddingStore, collection, fingerprint string) []PendingChunk {
	t.Helper()
	pending, err := embeddings.PendingChunks(t.Context(), collection, fingerprint, 0, 100)
	if err != nil {
		t.Fatalf("PendingChunks: %v", err)
	}
	return pending
}

func vectorWrites(chunks []PendingChunk, dimensions int) []VectorWrite {
	writes := make([]VectorWrite, 0, len(chunks))
	for i := range chunks {
		writes = append(writes, VectorWrite{RowID: chunks[i].RowID, ContentSHA256: chunks[i].SHA256, Vector: unitVector(dimensions, float64(chunks[i].RowID))})
	}
	return writes
}

func vectorRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var rows int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM kb_embedding`).Scan(&rows); err != nil {
		t.Fatalf("count vectors: %v", err)
	}
	return rows
}

func TestDisableEmbeddingRunsOnlyInsideATransaction(t *testing.T) {
	db, kb := newEmbeddingStore(t)
	ctx := t.Context()
	writeKBSource(t, kb, "support", "/docs/a.md", "alpha text")
	if err := kb.StartEmbedding(ctx, "support", embedFingerprintA); err != nil {
		t.Fatalf("StartEmbedding: %v", err)
	}
	if _, err := kb.CommitVectors(ctx, "support", embedFingerprintA, vectorWrites(pendingChunks(t, kb, "support", embedFingerprintA), 8)); err != nil {
		t.Fatalf("CommitVectors: %v", err)
	}
	_, err := kb.DisableEmbedding(ctx, "support")
	requireCode(t, err, ErrorCodeInvalidArgument)
	if rows := vectorRows(t, db); rows != 1 {
		t.Fatalf("vectors = %d, want 1 preserved after the refusal outside a transaction", rows)
	}
}

func TestDisableEmbeddingRollsBackWithItsTransaction(t *testing.T) {
	db, kb := newEmbeddingStore(t)
	ctx := t.Context()
	writeKBSource(t, kb, "support", "/docs/a.md", "alpha text")
	if err := kb.StartEmbedding(ctx, "support", embedFingerprintA); err != nil {
		t.Fatalf("StartEmbedding: %v", err)
	}
	if _, err := kb.CommitVectors(ctx, "support", embedFingerprintA, vectorWrites(pendingChunks(t, kb, "support", embedFingerprintA), 8)); err != nil {
		t.Fatalf("CommitVectors: %v", err)
	}
	abort := errors.New("abort")
	err := sqlite.RunInTx(ctx, db, func(txCtx context.Context) error {
		if _, err := kb.DisableEmbedding(txCtx, "support"); err != nil {
			return err
		}
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatalf("RunInTx = %v, want the abort", err)
	}
	if rows := vectorRows(t, db); rows != 1 {
		t.Fatalf("vectors = %d, want 1 kept by the rollback", rows)
	}
	if got := pragmaValue(t, db, "secure_delete"); got != "0" {
		t.Errorf("secure_delete leaked into the pool: %q", got)
	}
}

func TestKnowledgeEmbeddingSchemaAddsFingerprintAndCascadingVectors(t *testing.T) {
	db, kb := newEmbeddingStore(t)
	ctx := t.Context()
	present, err := EmbeddingPresent(ctx, db)
	if err != nil || !present {
		t.Fatalf("EmbeddingPresent = %v, %v", present, err)
	}
	writeKBSource(t, kb, "support", "/docs/a.md", "alpha text", "beta text")
	if err := kb.StartEmbedding(ctx, "support", embedFingerprintA); err != nil {
		t.Fatalf("StartEmbedding: %v", err)
	}
	if _, err := kb.CommitVectors(ctx, "support", embedFingerprintA, vectorWrites(pendingChunks(t, kb, "support", embedFingerprintA), 8)); err != nil {
		t.Fatalf("CommitVectors: %v", err)
	}
	if rows := vectorRows(t, db); rows != 2 {
		t.Fatalf("vectors = %d, want 2", rows)
	}
	if _, err := kb.ReplaceSource(ctx, kbWrite("support", "/docs/a.md", "b", kbChunks("alpha text", "beta text changed"))); err != nil {
		t.Fatalf("ReplaceSource: %v", err)
	}
	if rows := vectorRows(t, db); rows != 1 {
		t.Errorf("vectors after a chunk changed = %d, want the changed chunk's vector gone", rows)
	}
	var foreignKeys int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_list('kb_embedding') WHERE "table" = 'kb_chunk' AND on_delete = 'CASCADE'`).Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Errorf("cascade foreign key = %d, %v", foreignKeys, err)
	}
}

func TestKnowledgeEmbeddingPresentIsFalseBeforeTheSchema(t *testing.T) {
	db := newKBTestDB(t)
	for _, statement := range []string{`DROP TABLE kb_embedding`, `ALTER TABLE kb_collection DROP COLUMN embedding_fingerprint`} {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatalf("downgrade %q: %v", statement, err)
		}
	}
	present, err := EmbeddingPresent(t.Context(), db)
	if err != nil || present {
		t.Fatalf("EmbeddingPresent = %v, %v; want false without error", present, err)
	}
}

func TestKnowledgeEmbeddingRunBumpsGenerationOnceOnCompletion(t *testing.T) {
	db, kb := newEmbeddingStore(t)
	ctx := t.Context()
	writeKBSource(t, kb, "support", "/docs/a.md", "alpha text", "beta text", "gamma text")
	before := kbGeneration(t, db, "support")

	state, err := kb.EmbeddingState(ctx, "support", embedFingerprintA)
	if err != nil || !state.Exists || state.Fingerprint != "" || state.Chunks != 3 || state.Covered != 0 || state.PendingCharacters == 0 {
		t.Fatalf("EmbeddingState before = %+v, %v", state, err)
	}
	if err := kb.StartEmbedding(ctx, "support", embedFingerprintA); err != nil {
		t.Fatalf("StartEmbedding: %v", err)
	}
	if generation := kbGeneration(t, db, "support"); generation != before {
		t.Errorf("generation after opt-in = %d, want unchanged %d", generation, before)
	}
	pending := pendingChunks(t, kb, "support", embedFingerprintA)
	if len(pending) != 3 || pending[0].Heading != "Section" || pending[0].Content != "alpha text" {
		t.Fatalf("pending = %+v", pending)
	}

	first, err := kb.CommitVectors(ctx, "support", embedFingerprintA, vectorWrites(pending[:2], 8))
	if err != nil || first.Written != 2 || first.Completed {
		t.Fatalf("first commit = %+v, %v", first, err)
	}
	if generation := kbGeneration(t, db, "support"); generation != before {
		t.Errorf("generation after a partial batch = %d, want unchanged %d", generation, before)
	}
	state, _ = kb.EmbeddingState(ctx, "support", embedFingerprintA)
	if state.Covered != 2 || state.Fingerprint != embedFingerprintA {
		t.Errorf("EmbeddingState partial = %+v", state)
	}
	if rest := pendingChunks(t, kb, "support", embedFingerprintA); len(rest) != 1 || rest[0].RowID != pending[2].RowID {
		t.Fatalf("pending after the first batch = %+v", rest)
	}

	last, err := kb.CommitVectors(ctx, "support", embedFingerprintA, vectorWrites(pending[2:], 8))
	if err != nil || last.Written != 1 || !last.Completed {
		t.Fatalf("last commit = %+v, %v", last, err)
	}
	if generation := kbGeneration(t, db, "support"); generation != before+1 {
		t.Errorf("generation after completion = %d, want %d", generation, before+1)
	}
	if rest := pendingChunks(t, kb, "support", embedFingerprintA); len(rest) != 0 {
		t.Errorf("pending after completion = %+v", rest)
	}
}

func TestKnowledgeEmbeddingStoresLittleEndianUnitVectors(t *testing.T) {
	db, kb := newEmbeddingStore(t)
	ctx := t.Context()
	writeKBSource(t, kb, "support", "/docs/a.md", "alpha text")
	if err := kb.StartEmbedding(ctx, "support", embedFingerprintA); err != nil {
		t.Fatalf("StartEmbedding: %v", err)
	}
	pending := pendingChunks(t, kb, "support", embedFingerprintA)
	writes := vectorWrites(pending, 6)
	if _, err := kb.CommitVectors(ctx, "support", embedFingerprintA, writes); err != nil {
		t.Fatalf("CommitVectors: %v", err)
	}
	var blob []byte
	var fingerprint, digest string
	if err := db.QueryRowContext(ctx, `SELECT vector, fingerprint, content_sha256 FROM kb_embedding WHERE chunk_rowid = ?`, pending[0].RowID).Scan(&blob, &fingerprint, &digest); err != nil {
		t.Fatalf("read vector: %v", err)
	}
	if !bytes.Equal(blob, encodeVector(writes[0].Vector)) || len(blob) != 6*4 || fingerprint != embedFingerprintA || digest != pending[0].SHA256 {
		t.Fatalf("stored vector = %x fingerprint %q digest %q", blob, fingerprint, digest)
	}
	var length float64
	for i := 0; i < len(blob); i += 4 {
		value := math.Float32frombits(uint32(blob[i]) | uint32(blob[i+1])<<8 | uint32(blob[i+2])<<16 | uint32(blob[i+3])<<24)
		length += float64(value) * float64(value)
	}
	if math.Abs(math.Sqrt(length)-1) > 1e-5 {
		t.Errorf("stored vector length = %v, want 1", math.Sqrt(length))
	}
}

func TestKnowledgeEmbeddingRejectsInvalidVectorsBeforeWriting(t *testing.T) {
	db, kb := newEmbeddingStore(t)
	ctx := t.Context()
	writeKBSource(t, kb, "support", "/docs/a.md", "alpha text")
	if err := kb.StartEmbedding(ctx, "support", embedFingerprintA); err != nil {
		t.Fatalf("StartEmbedding: %v", err)
	}
	pending := pendingChunks(t, kb, "support", embedFingerprintA)
	good := unitVector(4, 1)
	tests := map[string]VectorWrite{
		"empty":        {RowID: pending[0].RowID, ContentSHA256: pending[0].SHA256, Vector: nil},
		"not unit":     {RowID: pending[0].RowID, ContentSHA256: pending[0].SHA256, Vector: []float32{1, 1, 1, 1}},
		"zero":         {RowID: pending[0].RowID, ContentSHA256: pending[0].SHA256, Vector: []float32{0, 0, 0, 0}},
		"nan":          {RowID: pending[0].RowID, ContentSHA256: pending[0].SHA256, Vector: []float32{float32(math.NaN()), 0, 0, 0}},
		"infinite":     {RowID: pending[0].RowID, ContentSHA256: pending[0].SHA256, Vector: []float32{float32(math.Inf(1)), 0, 0, 0}},
		"short digest": {RowID: pending[0].RowID, ContentSHA256: "abc", Vector: good},
	}
	for name, write := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := kb.CommitVectors(ctx, "support", embedFingerprintA, []VectorWrite{write})
			requireCode(t, err, ErrorCodeInvalidArgument)
		})
	}
	if _, err := kb.CommitVectors(ctx, "support", embedFingerprintA, nil); err == nil {
		t.Error("CommitVectors accepted an empty batch")
	}
	if rows := vectorRows(t, db); rows != 0 {
		t.Errorf("vectors = %d, want none", rows)
	}
}

func TestKnowledgeEmbeddingRejectsALateWriteAgainstAReusedRowID(t *testing.T) {
	db, kb := newEmbeddingStore(t)
	ctx := t.Context()
	writeKBSource(t, kb, "support", "/docs/a.md", "first body of text")
	if err := kb.StartEmbedding(ctx, "support", embedFingerprintA); err != nil {
		t.Fatalf("StartEmbedding: %v", err)
	}
	stale := pendingChunks(t, kb, "support", embedFingerprintA)

	if _, err := kb.ReplaceSource(ctx, kbWrite("support", "/docs/a.md", "b", kbChunks("second body of text"))); err != nil {
		t.Fatalf("ReplaceSource: %v", err)
	}
	fresh := pendingChunks(t, kb, "support", embedFingerprintA)
	if len(fresh) != 1 || fresh[0].RowID != stale[0].RowID || fresh[0].SHA256 == stale[0].SHA256 {
		t.Fatalf("rowid was not reused: stale %+v fresh %+v", stale, fresh)
	}

	_, err := kb.CommitVectors(ctx, "support", embedFingerprintA, vectorWrites(stale, 8))
	requireCode(t, err, ErrorCodeEmbeddingFailed)
	if rows := vectorRows(t, db); rows != 0 {
		t.Errorf("vectors = %d, want the stale write rejected", rows)
	}
	if _, err := kb.CommitVectors(ctx, "support", embedFingerprintA, vectorWrites(fresh, 8)); err != nil {
		t.Fatalf("current write rejected: %v", err)
	}
}

func TestKnowledgeEmbeddingRejectsAWriteAfterOptOutOrFingerprintChange(t *testing.T) {
	db, kb := newEmbeddingStore(t)
	ctx := t.Context()
	writeKBSource(t, kb, "support", "/docs/a.md", "alpha text", "beta text")
	if err := kb.StartEmbedding(ctx, "support", embedFingerprintA); err != nil {
		t.Fatalf("StartEmbedding: %v", err)
	}
	pending := pendingChunks(t, kb, "support", embedFingerprintA)

	_, err := kb.CommitVectors(ctx, "support", embedFingerprintB, vectorWrites(pending, 8))
	requireCode(t, err, ErrorCodeEmbeddingFailed)

	if _, _, err := kb.disableAndTruncate(ctx, "support"); err != nil {
		t.Fatalf("DisableEmbedding: %v", err)
	}
	_, err = kb.CommitVectors(ctx, "support", embedFingerprintA, vectorWrites(pending, 8))
	requireCode(t, err, ErrorCodeEmbeddingFailed)
	if rows := vectorRows(t, db); rows != 0 {
		t.Errorf("vectors = %d, want none after the opt-out", rows)
	}
}

func TestKnowledgeEmbeddingBatchIsAllOrNothing(t *testing.T) {
	db, kb := newEmbeddingStore(t)
	ctx := t.Context()
	writeKBSource(t, kb, "support", "/docs/a.md", "alpha text", "beta text")
	if err := kb.StartEmbedding(ctx, "support", embedFingerprintA); err != nil {
		t.Fatalf("StartEmbedding: %v", err)
	}
	writes := vectorWrites(pendingChunks(t, kb, "support", embedFingerprintA), 8)
	writes[1].ContentSHA256 = strings.Repeat("0", 64)
	_, err := kb.CommitVectors(ctx, "support", embedFingerprintA, writes)
	requireCode(t, err, ErrorCodeEmbeddingFailed)
	if rows := vectorRows(t, db); rows != 0 {
		t.Errorf("vectors = %d, want the whole batch rolled back", rows)
	}
}

func TestKnowledgeEmbeddingReembedReplacesVectorsOfAnotherFingerprint(t *testing.T) {
	db, kb := newEmbeddingStore(t)
	ctx := t.Context()
	writeKBSource(t, kb, "support", "/docs/a.md", "alpha text", "beta text")
	if err := kb.StartEmbedding(ctx, "support", embedFingerprintA); err != nil {
		t.Fatalf("StartEmbedding: %v", err)
	}
	if _, err := kb.CommitVectors(ctx, "support", embedFingerprintA, vectorWrites(pendingChunks(t, kb, "support", embedFingerprintA), 8)); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	state, _ := kb.EmbeddingState(ctx, "support", embedFingerprintB)
	if state.Covered != 0 || state.Fingerprint != embedFingerprintA {
		t.Fatalf("state under another fingerprint = %+v, want nothing covered", state)
	}
	if err := kb.StartEmbedding(ctx, "support", embedFingerprintB); err != nil {
		t.Fatalf("StartEmbedding B: %v", err)
	}
	pending := pendingChunks(t, kb, "support", embedFingerprintB)
	if len(pending) != 2 {
		t.Fatalf("pending under the new fingerprint = %d, want all chunks", len(pending))
	}
	result, err := kb.CommitVectors(ctx, "support", embedFingerprintB, vectorWrites(pending, 16))
	if err != nil || !result.Completed {
		t.Fatalf("second commit = %+v, %v", result, err)
	}
	var other int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM kb_embedding WHERE fingerprint <> ?`, embedFingerprintB).Scan(&other); err != nil || other != 0 {
		t.Errorf("vectors of the old fingerprint = %d, %v", other, err)
	}
	coverage, err := kb.EmbeddingCoverage(ctx, embedFingerprintA)
	if err != nil || len(coverage) != 1 || coverage[0].Covered != 0 || coverage[0].Chunks != 2 || coverage[0].Fingerprint != embedFingerprintB {
		t.Fatalf("coverage under the old fingerprint = %+v, %v", coverage, err)
	}
}

func TestKnowledgeEmbeddingCoverageReportsOptedInCollectionsOnly(t *testing.T) {
	_, kb := newEmbeddingStore(t)
	ctx := t.Context()
	writeKBSource(t, kb, "support", "/docs/a.md", "alpha text", "beta text")
	writeKBSource(t, kb, "other", "/docs/b.md", "gamma text")
	if err := kb.StartEmbedding(ctx, "support", embedFingerprintA); err != nil {
		t.Fatalf("StartEmbedding: %v", err)
	}
	pending := pendingChunks(t, kb, "support", embedFingerprintA)
	if _, err := kb.CommitVectors(ctx, "support", embedFingerprintA, vectorWrites(pending[:1], 8)); err != nil {
		t.Fatalf("CommitVectors: %v", err)
	}
	coverage, err := kb.EmbeddingCoverage(ctx, embedFingerprintA)
	if err != nil || len(coverage) != 1 || coverage[0] != (EmbeddingCoverage{Collection: "support", Fingerprint: embedFingerprintA, Chunks: 2, Covered: 1}) {
		t.Fatalf("coverage = %+v, %v", coverage, err)
	}
	empty, err := kb.EmbeddingCoverage(ctx, "")
	if err != nil || len(empty) != 1 || empty[0].Covered != 0 {
		t.Fatalf("coverage without a configured fingerprint = %+v, %v", empty, err)
	}
}

func TestKnowledgeEmbeddingOperationsReportAMissingCollection(t *testing.T) {
	_, kb := newEmbeddingStore(t)
	ctx := t.Context()
	state, err := kb.EmbeddingState(ctx, "missing", embedFingerprintA)
	if err != nil || state.Exists {
		t.Fatalf("EmbeddingState = %+v, %v; want not found without error", state, err)
	}
	requireCode(t, kb.StartEmbedding(ctx, "missing", embedFingerprintA), ErrorCodeSourceNotFound)
	_, err = kb.PreviewDisableEmbedding(ctx, "missing")
	requireCode(t, err, ErrorCodeSourceNotFound)
	_, _, err = kb.disableAndTruncate(ctx, "missing")
	requireCode(t, err, ErrorCodeSourceNotFound)
	if err := kb.StartEmbedding(ctx, "missing", ""); err == nil {
		t.Error("StartEmbedding accepted an empty fingerprint")
	}
	if _, err := kb.PendingChunks(ctx, "missing", embedFingerprintA, 0, 0); err == nil {
		t.Error("PendingChunks accepted a zero limit")
	}
}

func embedAndSeed(t *testing.T, kb *SQLiteStore, collection, uri, text string, seed float64) []float32 {
	t.Helper()
	writeKBSource(t, kb, collection, uri, text)
	if err := kb.StartEmbedding(t.Context(), collection, embedFingerprintA); err != nil {
		t.Fatalf("StartEmbedding: %v", err)
	}
	pending := pendingChunks(t, kb, collection, embedFingerprintA)
	vector := unitVector(1024, seed)
	if _, err := kb.CommitVectors(t.Context(), collection, embedFingerprintA, []VectorWrite{{RowID: pending[0].RowID, ContentSHA256: pending[0].SHA256, Vector: vector}}); err != nil {
		t.Fatalf("CommitVectors: %v", err)
	}
	return vector
}

func vectorMarkers(vector []float32) [][]byte {
	blob := encodeVector(vector)
	return [][]byte{blob[:256], blob[3000:3256]}
}

func requireVectorBytes(t *testing.T, db *sql.DB, markers [][]byte, wantInWAL bool) {
	t.Helper()
	walPath := databasePath(t, db) + "-wal"
	wal, err := os.ReadFile(walPath)
	if err != nil {
		t.Fatalf("read write-ahead log: %v", err)
	}
	database, err := os.ReadFile(databasePath(t, db))
	if err != nil {
		t.Fatalf("read database file: %v", err)
	}
	for _, marker := range markers {
		if got := bytes.Contains(wal, marker); got != wantInWAL {
			t.Fatalf("write-ahead log holds vector bytes = %v, want %v", got, wantInWAL)
		}
		if !wantInWAL && bytes.Contains(database, marker) {
			t.Error("vector bytes are still in the database file")
		}
	}
}

func TestKnowledgeDisableEmbeddingErasesVectorsFromDatabaseAndWriteAheadLog(t *testing.T) {
	db, kb := newEmbeddingStore(t)
	ctx := t.Context()
	vector := embedAndSeed(t, kb, "support", "/docs/a.md", "The quokkafernweed paragraph.", 1)
	writeKBSource(t, kb, "other", "/docs/b.md", "An unrelated paragraph that stays.")
	markers := vectorMarkers(vector)
	requireVectorBytes(t, db, markers, true)
	before := kbGeneration(t, db, "support")

	preview, err := kb.PreviewDisableEmbedding(ctx, "support")
	if err != nil || !preview.OptedIn || preview.Vectors != 1 {
		t.Fatalf("preview = %+v, %v", preview, err)
	}
	if rows := vectorRows(t, db); rows != 1 || kbGeneration(t, db, "support") != before {
		t.Fatal("the preview changed the collection")
	}

	result, truncated, err := kb.disableAndTruncate(ctx, "support")
	if err != nil || !truncated || !result.OptedIn || result.Vectors != 1 {
		t.Fatalf("DisableEmbedding = %+v, %v", result, err)
	}
	requireVectorBytes(t, db, markers, false)
	if rows := vectorRows(t, db); rows != 0 {
		t.Errorf("vectors = %d, want none", rows)
	}
	state, _ := kb.EmbeddingState(ctx, "support", embedFingerprintA)
	if state.Fingerprint != "" || state.Covered != 0 {
		t.Errorf("state after opt-out = %+v", state)
	}
	if generation := kbGeneration(t, db, "support"); generation != before+1 {
		t.Errorf("generation = %d, want one bump to %d", generation, before+1)
	}
	if got := pragmaValue(t, db, "secure_delete"); got != "0" {
		t.Errorf("secure_delete leaked into the pool: %q", got)
	}

	again, _, err := kb.disableAndTruncate(ctx, "support")
	if err != nil || again.OptedIn || again.Vectors != 0 {
		t.Fatalf("second DisableEmbedding = %+v, %v", again, err)
	}
	if generation := kbGeneration(t, db, "support"); generation != before+1 {
		t.Errorf("generation after a no-op opt-out = %d, want unchanged %d", generation, before+1)
	}
	kbCheckClean(t, kb)
}

func TestKnowledgeRemoveSourceErasesVectorsFromDatabaseAndWriteAheadLog(t *testing.T) {
	db, kb := newEmbeddingStore(t)
	ctx := t.Context()
	vector := embedAndSeed(t, kb, "support", "/docs/a.md", "The quokkafernweed paragraph.", 2)
	writeKBSource(t, kb, "support", "/docs/b.md", "An unrelated paragraph that stays.")
	markers := vectorMarkers(vector)
	requireVectorBytes(t, db, markers, true)

	result, truncated, err := kb.removeAndTruncate(ctx, "support", "/docs/a.md")
	if err != nil || !truncated {
		t.Fatalf("DeleteSource = %+v, %v", result, err)
	}
	requireVectorBytes(t, db, markers, false)
	if rows := vectorRows(t, db); rows != 0 {
		t.Errorf("vectors = %d, want the cascade to remove them", rows)
	}
}
