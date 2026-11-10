package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var kbTestTime = time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)

func kbChunks(texts ...string) []KBChunk {
	chunks := make([]KBChunk, 0, len(texts))
	for i, text := range texts {
		chunks = append(chunks, KBChunk{Ordinal: i, Heading: "Section", Content: text, SHA256: kbDigest(text)})
	}
	return chunks
}

func kbDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func kbWrite(collection, uri, digest string, chunks []KBChunk) *KBSourceWrite {
	return &KBSourceWrite{
		Collection: collection, Kind: KBSourceKindFile, URI: uri, Label: "notes.md",
		ContentSHA256: strings.Repeat(digest, 64)[:64], IngestVersion: 1, IngestedAt: kbTestTime,
		Chunks: chunks, Limits: KBLimits{CollectionChunks: 100, TotalChunks: 200},
	}
}

func kbIndexHits(t *testing.T, db *sql.DB, match string) int {
	t.Helper()
	var hits int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM kb_chunk_fts WHERE kb_chunk_fts MATCH ?`, match).Scan(&hits); err != nil {
		t.Fatalf("match %q: %v", match, err)
	}
	return hits
}

func kbCheckClean(t *testing.T, kb KBStore) {
	t.Helper()
	report, err := kb.Check(t.Context())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Problems) != 0 {
		t.Fatalf("Check problems = %v", report.Problems)
	}
}

func TestKnowledgeSchemaHasSecureDeletePorterAndScope(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	for _, name := range []string{"kb_collection", "kb_source", "kb_chunk", "kb_chunk_fts"} {
		var found string
		if err := db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE name = ?`, name).Scan(&found); err != nil {
			t.Errorf("%s missing: %v", name, err)
		}
	}
	for _, trigger := range []string{"kb_chunk_fts_insert", "kb_chunk_fts_delete", "kb_chunk_fts_update"} {
		var found string
		if err := db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'trigger' AND name = ?`, trigger).Scan(&found); err != nil {
			t.Errorf("trigger %s missing: %v", trigger, err)
		}
	}
	var secureDelete int
	if err := db.QueryRowContext(ctx, `SELECT v FROM kb_chunk_fts_config WHERE k = 'secure-delete'`).Scan(&secureDelete); err != nil || secureDelete != 1 {
		t.Fatalf("secure-delete option = %d, %v; want 1", secureDelete, err)
	}
	kb := NewKBStore(db)
	if _, err := kb.ReplaceSource(ctx, kbWrite("support", "/docs/policy.md", "a", kbChunks("Customers may request a Refund within thirty days."))); err != nil {
		t.Fatalf("ReplaceSource: %v", err)
	}
	if hits := kbIndexHits(t, db, `"refunds"`); hits != 1 {
		t.Errorf("stemmed match hits = %d, want 1", hits)
	}
	if hits := kbIndexHits(t, db, `scope:`+KBScope("support")); hits != 1 {
		t.Errorf("scope token hits = %d, want 1", hits)
	}
	if hits := kbIndexHits(t, db, `scope:`+KBScope("other")); hits != 0 {
		t.Errorf("foreign scope token hits = %d, want 0", hits)
	}
}

func TestKnowledgeScopeSeparatesCollectionsThatDifferByHyphenOrStem(t *testing.T) {
	scopes := map[string]string{}
	for _, id := range []string{"hr-docs", "hrdocs", "hr-doc", "refund", "refunds"} {
		scope := KBScope(id)
		if previous, clash := scopes[scope]; clash {
			t.Fatalf("scope clash between %q and %q", previous, id)
		}
		scopes[scope] = id
		if !strings.HasPrefix(scope, "kb") || !strings.HasSuffix(scope, "0") || len(scope) != 2+32+1 {
			t.Errorf("scope %q has unexpected shape", scope)
		}
	}
}

func TestKnowledgeReplaceSourceDiffsByChunkHash(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	kb := NewKBStore(db)

	added, err := kb.ReplaceSource(ctx, kbWrite("support", "/docs/a.md", "a", kbChunks("alpha text", "beta text", "gamma text")))
	if err != nil || added.Status != KBWriteAdded || added.ChunksAdded != 3 || added.ChunksKept != 0 {
		t.Fatalf("first write = %+v, %v", added, err)
	}
	idsBefore := kbChunkIDs(t, db)

	unchanged, err := kb.ReplaceSource(ctx, kbWrite("support", "/docs/a.md", "a", kbChunks("alpha text", "beta text", "gamma text")))
	if err != nil || unchanged.Status != KBWriteUnchanged || unchanged.ChunksAdded != 0 {
		t.Fatalf("unchanged write = %+v, %v", unchanged, err)
	}
	if generation := kbGeneration(t, db, "support"); generation != 1 {
		t.Errorf("generation after no-op = %d, want 1", generation)
	}

	replaced, err := kb.ReplaceSource(ctx, kbWrite("support", "/docs/a.md", "b", kbChunks("alpha text", "beta text changed", "gamma text")))
	if err != nil || replaced.Status != KBWriteReplaced || replaced.ChunksAdded != 1 || replaced.ChunksRemoved != 1 || replaced.ChunksKept != 2 {
		t.Fatalf("replace write = %+v, %v", replaced, err)
	}
	idsAfter := kbChunkIDs(t, db)
	for _, ordinal := range []int{0, 2} {
		if idsBefore[ordinal] != idsAfter[ordinal] {
			t.Errorf("chunk at ordinal %d was rewritten although its hash is unchanged", ordinal)
		}
	}
	if idsBefore[1] == idsAfter[1] {
		t.Error("chunk at ordinal 1 kept its id although its content changed")
	}
	if generation := kbGeneration(t, db, "support"); generation != 2 {
		t.Errorf("generation after replace = %d, want 2", generation)
	}
	if hits := kbIndexHits(t, db, `"changed"`); hits != 1 {
		t.Errorf("new text hits = %d, want 1", hits)
	}
	kbCheckClean(t, kb)
}

func TestKnowledgeReplaceSourceHandlesMovedChunksWithoutOrdinalCollision(t *testing.T) {
	db := newTestDB(t)
	kb := NewKBStore(db)
	ctx := t.Context()
	if _, err := kb.ReplaceSource(ctx, kbWrite("support", "/docs/a.md", "a", kbChunks("alpha", "beta", "gamma"))); err != nil {
		t.Fatalf("first write: %v", err)
	}
	moved, err := kb.ReplaceSource(ctx, kbWrite("support", "/docs/a.md", "b", kbChunks("gamma", "alpha", "beta")))
	if err != nil {
		t.Fatalf("moved write: %v", err)
	}
	if moved.ChunksAdded != 3 || moved.ChunksRemoved != 3 {
		t.Errorf("moved write = %+v, want every chunk rewritten", moved)
	}
	kbCheckClean(t, kb)
}

func TestKnowledgeReplaceSourceRejectsCapsAndKeepsOldChunks(t *testing.T) {
	db := newTestDB(t)
	kb := NewKBStore(db)
	ctx := t.Context()
	if _, err := kb.ReplaceSource(ctx, kbWrite("support", "/docs/a.md", "a", kbChunks("alpha", "beta"))); err != nil {
		t.Fatalf("first write: %v", err)
	}
	tooMany := kbWrite("support", "/docs/a.md", "b", kbChunks("one", "two", "three"))
	tooMany.Limits.CollectionChunks = 2
	if _, err := kb.ReplaceSource(ctx, tooMany); !hasStoreCode(err, ErrorCodeKBCapExceeded) {
		t.Fatalf("collection cap error = %v, want %s", err, ErrorCodeKBCapExceeded)
	}
	other := kbWrite("support", "/docs/b.md", "c", kbChunks("delta"))
	other.Limits.CollectionChunks = 2
	if _, err := kb.ReplaceSource(ctx, other); !hasStoreCode(err, ErrorCodeKBCapExceeded) {
		t.Fatalf("second source collection cap error = %v, want %s", err, ErrorCodeKBCapExceeded)
	}
	total := kbWrite("elsewhere", "/docs/c.md", "d", kbChunks("epsilon"))
	total.Limits.TotalChunks = 2
	if _, err := kb.ReplaceSource(ctx, total); !hasStoreCode(err, ErrorCodeKBCapExceeded) {
		t.Fatalf("total cap error = %v, want %s", err, ErrorCodeKBCapExceeded)
	}
	sources, err := kb.ListSources(ctx, "support")
	if err != nil || len(sources) != 1 || sources[0].Chunks != 2 {
		t.Fatalf("sources after rejected writes = %+v, %v; want the original two chunks", sources, err)
	}
	if hits := kbIndexHits(t, db, `"alpha"`); hits != 1 {
		t.Errorf("old chunk hits = %d, want 1", hits)
	}
	kbCheckClean(t, kb)
}

func TestKnowledgeReplaceSourceRejectsInvalidWrites(t *testing.T) {
	kb := NewKBStore(newTestDB(t))
	cases := []struct {
		name   string
		mutate func(*KBSourceWrite)
	}{
		{"empty collection", func(w *KBSourceWrite) { w.Collection = "" }},
		{"unknown kind", func(w *KBSourceWrite) { w.Kind = "ftp" }},
		{"short digest", func(w *KBSourceWrite) { w.ContentSHA256 = "abc" }},
		{"no chunks", func(w *KBSourceWrite) { w.Chunks = nil }},
		{"sparse ordinals", func(w *KBSourceWrite) { w.Chunks[0].Ordinal = 4 }},
		{"empty content", func(w *KBSourceWrite) { w.Chunks[0].Content = "" }},
		{"no limits", func(w *KBSourceWrite) { w.Limits = KBLimits{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			write := kbWrite("support", "/docs/a.md", "a", kbChunks("alpha"))
			tc.mutate(write)
			if _, err := kb.ReplaceSource(t.Context(), write); !hasStoreCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("error = %v, want %s", err, ErrorCodeInvalidArgument)
			}
		})
	}
	if _, err := kb.ReplaceSource(t.Context(), nil); !hasStoreCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("nil write error = %v, want %s", err, ErrorCodeInvalidArgument)
	}
}

func TestKnowledgeCascadeDeleteKeepsIndexConsistent(t *testing.T) {
	db := newTestDB(t)
	kb := NewKBStore(db)
	ctx := t.Context()
	if _, err := kb.ReplaceSource(ctx, kbWrite("support", "/docs/a.md", "a", kbChunks("alpha unique", "beta unique"))); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if _, err := kb.ReplaceSource(ctx, kbWrite("support", "/docs/b.md", "b", kbChunks("gamma unique"))); err != nil {
		t.Fatalf("write b: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM kb_source WHERE uri = '/docs/a.md'`); err != nil {
		t.Fatalf("cascade delete: %v", err)
	}
	if hits := kbIndexHits(t, db, `"unique"`); hits != 1 {
		t.Errorf("hits after cascade = %d, want only the surviving chunk", hits)
	}
	kbCheckClean(t, kb)
}

func TestKnowledgeRemoveSourceDryRunChangesNothing(t *testing.T) {
	db := newTestDB(t)
	kb := NewKBStore(db)
	ctx := t.Context()
	if _, err := kb.ReplaceSource(ctx, kbWrite("support", "/docs/a.md", "a", kbChunks("alpha", "beta"))); err != nil {
		t.Fatalf("write: %v", err)
	}
	result, err := kb.RemoveSource(ctx, "support", "/docs/a.md", false)
	if err != nil || result.Applied || result.Chunks != 2 {
		t.Fatalf("dry run = %+v, %v", result, err)
	}
	if generation := kbGeneration(t, db, "support"); generation != 1 {
		t.Errorf("generation after dry run = %d, want 1", generation)
	}
	if _, err := kb.RemoveSource(ctx, "support", "/docs/missing.md", false); !hasStoreCode(err, ErrorCodeKBSourceNotFound) {
		t.Fatalf("missing dry run error = %v, want %s", err, ErrorCodeKBSourceNotFound)
	}
	if _, err := kb.RemoveSource(ctx, "support", "/docs/missing.md", true); !hasStoreCode(err, ErrorCodeKBSourceNotFound) {
		t.Fatalf("missing apply error = %v, want %s", err, ErrorCodeKBSourceNotFound)
	}
	sources, err := kb.ListSources(ctx, "support")
	if err != nil || len(sources) != 1 {
		t.Fatalf("sources = %+v, %v", sources, err)
	}
}

func TestKnowledgeCollectionOutlivesItsLastSourceAndKeepsGeneration(t *testing.T) {
	db := newTestDB(t)
	kb := NewKBStore(db)
	ctx := t.Context()
	if _, err := kb.ReplaceSource(ctx, kbWrite("support", "/docs/a.md", "a", kbChunks("alpha"))); err != nil {
		t.Fatalf("write: %v", err)
	}
	listed, err := kb.ListCollections(ctx)
	if err != nil || len(listed) != 1 || listed[0].ID != "support" || listed[0].Sources != 1 || listed[0].Chunks != 1 {
		t.Fatalf("ListCollections = %+v, %v", listed, err)
	}
	result, err := kb.RemoveSource(ctx, "support", "/docs/a.md", true)
	if err != nil || !result.Applied || result.Chunks != 1 {
		t.Fatalf("RemoveSource = %+v, %v", result, err)
	}
	listed, err = kb.ListCollections(ctx)
	if err != nil || len(listed) != 0 {
		t.Fatalf("empty collection listed = %+v, %v", listed, err)
	}
	if generation := kbGeneration(t, db, "support"); generation != 2 {
		t.Errorf("generation after removal = %d, want 2", generation)
	}
	if _, err := kb.ReplaceSource(ctx, kbWrite("support", "/docs/a.md", "a", kbChunks("alpha"))); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if generation := kbGeneration(t, db, "support"); generation != 3 {
		t.Errorf("generation after re-add = %d, want 3", generation)
	}
}

func TestKnowledgeCheckDetectsDamagedIndex(t *testing.T) {
	cases := []struct {
		name   string
		damage []string
	}{
		{"deletion bypassing the trigger", []string{`DROP TRIGGER kb_chunk_fts_delete`, `DELETE FROM kb_chunk WHERE ordinal = 0`}},
		{"insertion bypassing the trigger", []string{`DROP TRIGGER kb_chunk_fts_insert`, `INSERT INTO kb_chunk (id, source_id, ordinal, heading, content, scope, content_sha256) SELECT 'rogue', source_id, 9, NULL, 'rogue text', scope, 'x' FROM kb_chunk LIMIT 1`}},
		{"index rows removed", []string{`DELETE FROM kb_chunk_fts_data WHERE id > 10`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			kb := NewKBStore(db)
			ctx := t.Context()
			if _, err := kb.ReplaceSource(ctx, kbWrite("support", "/docs/a.md", "a", kbChunks("alpha text", "beta text"))); err != nil {
				t.Fatalf("write: %v", err)
			}
			kbCheckClean(t, kb)
			for _, statement := range tc.damage {
				if _, err := db.ExecContext(ctx, statement); err != nil {
					t.Fatalf("damage %q: %v", statement, err)
				}
			}
			report, err := kb.Check(ctx)
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if len(report.Problems) == 0 {
				t.Fatal("Check reported a damaged index as healthy")
			}
		})
	}
}

func TestKnowledgeCheckDetectsScopeMismatch(t *testing.T) {
	db := newTestDB(t)
	kb := NewKBStore(db)
	ctx := t.Context()
	if _, err := kb.ReplaceSource(ctx, kbWrite("support", "/docs/a.md", "a", kbChunks("alpha text"))); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE kb_chunk SET scope = ?`, KBScope("other")); err != nil {
		t.Fatalf("update scope: %v", err)
	}
	report, err := kb.Check(ctx)
	if err != nil || len(report.Problems) != 1 {
		t.Fatalf("Check = %+v, %v; want one scope problem", report, err)
	}
}

func TestKnowledgeSourceNeverUsesInsertOrReplace(t *testing.T) {
	forbidden := regexp.MustCompile(`(?i)(insert\s+or\s+replace|replace\s+into)\s+(into\s+)?kb_`)
	for _, path := range []string{"knowledge.go", "migrate.go", filepath.Join("..", "modules", "knowledge", "service.go")} {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if forbidden.Match(content) {
			t.Errorf("%s replaces rows in a knowledge table, which skips the index delete trigger", path)
		}
	}
}

func TestKnowledgeSourceRequiresItsCollection(t *testing.T) {
	db := newTestDB(t)
	_, err := db.ExecContext(t.Context(),
		`INSERT INTO kb_source (id, collection, kind, uri, label, content_sha256, ingest_version, ingested_at) VALUES ('s', 'missing', 'file', '/x.md', 'x', 'd', 1, 'now')`)
	if err == nil {
		t.Fatal("source accepted without its collection")
	}
}

func TestKnowledgeRemoveErasesTermFromIndexAndWriteAheadLog(t *testing.T) {
	db := newTestDB(t)
	kb := NewKBStore(db)
	ctx := t.Context()
	const term = "quokkafernweed"
	const termStem = "quokkafern"
	if _, err := kb.ReplaceSource(ctx, kbWrite("support", "/docs/a.md", "a", kbChunks("The "+term+" appears in this paragraph only."))); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := kb.ReplaceSource(ctx, kbWrite("support", "/docs/b.md", "b", kbChunks("An unrelated paragraph that stays."))); err != nil {
		t.Fatalf("write unrelated: %v", err)
	}
	walPath := databasePath(t, db) + "-wal"
	if !indexBlocksContain(t, db, termStem) {
		t.Fatal("index does not hold the term before removal")
	}
	if wal, err := os.ReadFile(walPath); err != nil || !bytes.Contains(wal, []byte(term)) {
		t.Fatalf("write-ahead log does not hold the term before removal: %v", err)
	}

	result, err := kb.RemoveSource(ctx, "support", "/docs/a.md", true)
	if err != nil || !result.Applied || !result.WALTruncated {
		t.Fatalf("RemoveSource = %+v, %v", result, err)
	}

	if indexBlocksContain(t, db, termStem) {
		t.Error("term is still present in kb_chunk_fts_data after removal")
	}
	wal, err := os.ReadFile(walPath)
	if err != nil {
		t.Fatalf("read write-ahead log: %v", err)
	}
	if bytes.Contains(wal, []byte(term)) || bytes.Contains(wal, []byte(termStem)) {
		t.Error("term is still present in the write-ahead log after removal")
	}
	database, err := os.ReadFile(databasePath(t, db))
	if err != nil {
		t.Fatalf("read database file: %v", err)
	}
	if bytes.Contains(database, []byte(term)) {
		t.Error("term is still present in the database file after removal")
	}
	if got := pragmaValue(t, db, "secure_delete"); got != "0" {
		t.Errorf("secure_delete leaked into the pool: %q", got)
	}
	kbCheckClean(t, kb)
}

func databasePath(t *testing.T, db *sql.DB) string {
	t.Helper()
	var seq int
	var name, file string
	if err := db.QueryRowContext(t.Context(), `PRAGMA database_list`).Scan(&seq, &name, &file); err != nil {
		t.Fatalf("database_list: %v", err)
	}
	return file
}

func indexBlocksContain(t *testing.T, db *sql.DB, needle string) bool {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT block FROM kb_chunk_fts_data`)
	if err != nil {
		t.Fatalf("read index blocks: %v", err)
	}
	defer func() { _ = rows.Close() }()
	found := false
	for rows.Next() {
		var block []byte
		if err := rows.Scan(&block); err != nil {
			t.Fatalf("scan block: %v", err)
		}
		if bytes.Contains(block, []byte(needle)) {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read index blocks: %v", err)
	}
	return found
}

func kbChunkIDs(t *testing.T, db *sql.DB) map[int]string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT ordinal, id FROM kb_chunk`)
	if err != nil {
		t.Fatalf("list chunk ids: %v", err)
	}
	defer func() { _ = rows.Close() }()
	ids := map[int]string{}
	for rows.Next() {
		var ordinal int
		var id string
		if err := rows.Scan(&ordinal, &id); err != nil {
			t.Fatalf("scan chunk id: %v", err)
		}
		ids[ordinal] = id
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list chunk ids: %v", err)
	}
	return ids
}

func kbGeneration(t *testing.T, db *sql.DB, collection string) int64 {
	t.Helper()
	var generation int64
	if err := db.QueryRowContext(context.Background(), `SELECT generation FROM kb_collection WHERE id = ?`, collection).Scan(&generation); err != nil {
		t.Fatalf("read generation: %v", err)
	}
	return generation
}
