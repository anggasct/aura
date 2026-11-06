package cli

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/knowledge"
	"github.com/anggasct/aura/internal/store"
)

type kbRetrievalRig struct {
	cfg       *config.Config
	db        *sql.DB
	retriever *knowledge.Retriever
	service   *knowledge.Service
}

func newKBRetrievalRig(t *testing.T) *kbRetrievalRig {
	t.Helper()
	cfg, db, dbPath := kbTestStorage(t, writeMemoryCLIConfig(t))
	pool, err := openKnowledgeReadPool(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("openKnowledgeReadPool: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	retriever, err := newKnowledgeRetriever(cfg, pool)
	if err != nil {
		t.Fatalf("newKnowledgeRetriever: %v", err)
	}
	service, err := newKnowledgeService(cfg, db, nil)
	if err != nil {
		t.Fatalf("newKnowledgeService: %v", err)
	}
	return &kbRetrievalRig{cfg: cfg, db: db, retriever: retriever, service: service}
}

func (r *kbRetrievalRig) add(t *testing.T, collection, name, content string) {
	t.Helper()
	if _, err := r.service.Add(t.Context(), collection, writeKBDocument(t, name, content)); err != nil {
		t.Fatalf("Add %s: %v", name, err)
	}
}

func (r *kbRetrievalRig) search(t *testing.T, scope []string, query string) []knowledge.Hit {
	t.Helper()
	hits, err := r.retriever.Search(t.Context(), scope, query, knowledge.SearchDepth)
	if err != nil {
		t.Fatalf("Search(%q): %v", query, err)
	}
	return hits
}

func TestKnowledgeSearchFindsStemmedTermsInASmallCollection(t *testing.T) {
	rig := newKBRetrievalRig(t)
	rig.add(t, "support", "policy.md", "# Returns\nCustomers can request refunds within thirty days of purchase.\n\n# Shipping\nOrders ship in five days.\n\n# Warranty\nOne year.\n\n# Contact\nWrite to support.\n\n# About\nWe sell tools.\n")

	hits := rig.search(t, []string{"support"}, "What is your refund policy?")
	if len(hits) == 0 || !strings.Contains(hits[0].Content, "refunds") || hits[0].Label != "policy.md" || hits[0].Heading != "Returns" {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestKnowledgeSearchTreatsFTSSyntaxInUserTextAsPlainWords(t *testing.T) {
	rig := newKBRetrievalRig(t)
	rig.add(t, "support", "policy.md", "Refunds are issued within thirty days.")
	rig.add(t, "private", "plans.md", "Confidential merger plans for next year.")

	for _, query := range []string{
		`refund" OR "merger`, `refund OR merger*`, `scope:` + "x" + ` refund`, `NEAR(refund merger)`, `refund) AND (`, `"unterminated refund`, `-refund`, `refund ^ merger`,
	} {
		t.Run(query, func(t *testing.T) {
			for _, hit := range rig.search(t, []string{"support"}, query) {
				if hit.Collection != "support" || strings.Contains(hit.Content, "merger") {
					t.Errorf("query %q reached %+v", query, hit)
				}
			}
		})
	}
}

func TestKnowledgeSearchNeverCrossesCollectionsEvenWithTheScopeTokenInTheQuery(t *testing.T) {
	rig := newKBRetrievalRig(t)
	rig.add(t, "hr-docs", "a.md", "The refund claim form is on the intranet.")
	rig.add(t, "hrdocs", "b.md", "The refund desk is closed on Sundays.")
	rig.add(t, "refund", "c.md", "Refund policy for customers.")
	rig.add(t, "refunds", "d.md", "Refunds take five days.")

	scopeTokens := func(collections ...string) string {
		var tokens []string
		for _, collection := range collections {
			tokens = append(tokens, store.KBScope(collection))
		}
		return strings.Join(tokens, " ")
	}
	tests := map[string]struct {
		scope []string
		query string
		want  map[string]bool
	}{
		"hyphen":         {scope: []string{"hr-docs"}, query: "refund", want: map[string]bool{"hr-docs": true}},
		"no hyphen":      {scope: []string{"hrdocs"}, query: "refund", want: map[string]bool{"hrdocs": true}},
		"stem singular":  {scope: []string{"refund"}, query: "refunds", want: map[string]bool{"refund": true}},
		"stem plural":    {scope: []string{"refunds"}, query: "refund", want: map[string]bool{"refunds": true}},
		"scope token":    {scope: []string{"hrdocs"}, query: "refund " + scopeTokens("hr-docs", "refund", "refunds"), want: map[string]bool{"hrdocs": true}},
		"other scope":    {scope: []string{"refund"}, query: scopeTokens("refunds"), want: map[string]bool{}},
		"two collection": {scope: []string{"hr-docs", "refunds"}, query: "refund", want: map[string]bool{"hr-docs": true, "refunds": true}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			for _, hit := range rig.search(t, test.scope, test.query) {
				if !test.want[hit.Collection] {
					t.Errorf("scope %v returned a hit from %q", test.scope, hit.Collection)
				}
			}
		})
	}
}

func TestKnowledgeSearchDropsAChunkMatchingANewlyAddedVaultValue(t *testing.T) {
	rig := newKBRetrievalRig(t)
	const secretValue = "kb-late-secret-value-0123456789"
	rig.add(t, "support", "policy.md", "Refunds are issued within thirty days. The ledger key is "+secretValue+" for finance.")
	rig.add(t, "support", "clean.md", "Refunds can also be requested by phone.")

	if hits := rig.search(t, []string{"support"}, "refund"); len(hits) != 2 {
		t.Fatalf("hits before the secret exists = %d, want 2", len(hits))
	}
	t.Setenv("KB_TEST_LATE_SECRET", secretValue)
	rig.cfg.Webhook.Keys = []config.WebhookKey{{ID: "late", SecretEnv: "KB_TEST_LATE_SECRET"}}
	hits := rig.search(t, []string{"support"}, "refund")
	if len(hits) != 1 || hits[0].Label != "clean.md" {
		t.Fatalf("hits after the secret exists = %+v, want only clean.md", hits)
	}
}

func TestKnowledgeSearchFailsClosedWhenASecretReferenceIsUnresolvable(t *testing.T) {
	rig := newKBRetrievalRig(t)
	rig.add(t, "support", "policy.md", "Refunds are issued within thirty days.")
	rig.cfg.Webhook.Keys = []config.WebhookKey{{ID: "late", SecretEnv: "KB_TEST_UNSET_RETRIEVAL_SECRET"}}
	hits, err := rig.retriever.Search(t.Context(), []string{"support"}, "refund", knowledge.SearchDepth)
	if code, ok := knowledge.CodeOf(err); !ok || code != knowledge.ErrorCodeSecretUnavailable || len(hits) != 0 {
		t.Fatalf("hits = %v, error = %v", hits, err)
	}
}

func seedBulkKBScope(t testing.TB, db *sql.DB, collection string, chunks int, contentSQL string) {
	t.Helper()
	ctx := t.Context()
	stamp := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	if _, err := db.ExecContext(ctx, `INSERT INTO kb_collection (id, generation, created_at) VALUES (?, 0, ?) ON CONFLICT(id) DO NOTHING`, collection, stamp); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	sourceID := "kbs_bulk_" + collection
	if _, err := db.ExecContext(ctx,
		`INSERT INTO kb_source (id, collection, kind, uri, label, content_sha256, ingest_version, ingested_at) VALUES (?, ?, 'file', ?, 'bulk.md', ?, 1, ?)`,
		sourceID, collection, "/bulk/"+collection+".md", strings.Repeat("0", 64), stamp); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	query := fmt.Sprintf(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < %d)
		INSERT INTO kb_chunk (id, source_id, ordinal, heading, content, scope, content_sha256)
		SELECT ? || i, ?, i, 'Section', %s, ?, ? FROM n`, chunks, contentSQL)
	if _, err := db.ExecContext(ctx, query, "kbc_"+collection+"_", sourceID, store.KBScope(collection), strings.Repeat("0", 64)); err != nil {
		t.Fatalf("seed chunks: %v", err)
	}
}

func TestKnowledgeSearchFindsTheRefundChunkInAHundredThousandChunkCollection(t *testing.T) {
	rig := newKBRetrievalRig(t)
	seedBulkKBScope(t, rig.db, "bulk", knowledge.MaxChunksPerCollection-1, `'general information about topic ' || (i % 50) || ' and item number ' || i || ' in the catalogue'`)
	rig.add(t, "bulk", "policy.md", "Customers may request refunds within thirty days of purchase.")

	hits := rig.search(t, []string{"bulk"}, "What is your refund policy?")
	if len(hits) != 1 || hits[0].Label != "policy.md" || !strings.Contains(hits[0].Content, "refunds") {
		t.Fatalf("hits = %+v", hits)
	}
}
