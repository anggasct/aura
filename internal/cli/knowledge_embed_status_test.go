package cli

import (
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/embedding"
	"github.com/anggasct/aura/internal/health"
	"github.com/anggasct/aura/internal/store"
)

func embeddingFindings(t *testing.T, rig *kbRetrievalRig) []health.Finding {
	t.Helper()
	registry, err := buildHealthRegistry(rig.cfg, nil, func() (bool, string) { return true, "ok" }, func() (health.ProcessStatus, bool) { return health.ProcessStatus{}, false })
	if err != nil {
		t.Fatalf("buildHealthRegistry: %v", err)
	}
	var found []health.Finding
	all := registry.Evaluate(t.Context())
	for i := range all {
		if all[i].Component == health.ComponentKnowledgeEmbedding {
			found = append(found, all[i])
		}
	}
	return found
}

func statusEmbeddingConfig() *config.Knowledge {
	return &config.Knowledge{Embedding: &config.KnowledgeEmbedding{
		BaseURL: "https://api.example.com/v1", Model: "status-model", Dimensions: 4, DimensionsField: "dimensions",
		RoleStyle: config.EmbeddingRoleNone, BatchSize: 96, MaxMemory: config.DefaultEmbeddingMaxMemory,
	}}
}

func embedChunksForStatus(t *testing.T, rig *kbRetrievalRig, collection string, count int) {
	t.Helper()
	fingerprint, err := embedding.Fingerprint(rig.cfg.Knowledge.Embedding)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	vectors := store.NewKBEmbeddingStore(rig.db)
	if err := vectors.StartEmbedding(t.Context(), collection, fingerprint); err != nil {
		t.Fatalf("StartEmbedding: %v", err)
	}
	pending, err := vectors.PendingChunks(t.Context(), collection, fingerprint, 0, count)
	if err != nil {
		t.Fatalf("PendingChunks: %v", err)
	}
	writes := make([]store.KBVectorWrite, 0, len(pending))
	for i := range pending {
		writes = append(writes, store.KBVectorWrite{RowID: pending[i].RowID, ContentSHA256: pending[i].SHA256, Vector: []float32{1, 0, 0, 0}})
	}
	if _, err := vectors.CommitVectors(t.Context(), collection, fingerprint, writes); err != nil {
		t.Fatalf("CommitVectors: %v", err)
	}
}

func TestStatusSaysLexicalOnlyWithoutEmbeddingConfiguration(t *testing.T) {
	rig := newKnowledgeStatusRig(t)
	rig.add(t, "support", "a.md", "Refunds are issued within thirty days.")
	findings := embeddingFindings(t, rig)
	if len(findings) != 1 || findings[0].Code != "knowledge_lexical_only" || findings[0].Status != health.StatusUp ||
		!strings.Contains(findings[0].Detail, "not configured") {
		t.Fatalf("findings = %+v, want lexical only", findings)
	}
}

func TestStatusReportsAnUnresolvedEmbeddingCredentialAsLexicalOnly(t *testing.T) {
	rig := newKnowledgeStatusRig(t)
	rig.cfg.Knowledge = statusEmbeddingConfig()
	rig.cfg.Knowledge.Embedding.CredentialRef = "env://KB_STATUS_UNSET_KEY"
	findings := embeddingFindings(t, rig)
	if len(findings) != 1 || findings[0].Code != "knowledge_lexical_only" || !strings.Contains(findings[0].Detail, "credential could not be resolved") {
		t.Fatalf("findings = %+v", findings)
	}
}

func TestStatusReportsCoveragePartialReadyAndReembedRequired(t *testing.T) {
	rig := newKnowledgeStatusRig(t)
	rig.cfg.Knowledge = statusEmbeddingConfig()
	rig.add(t, "support", "a.md", "# One\nFirst body.\n\n# Two\nSecond body.\n")
	rig.add(t, "faq", "b.md", "# Hours\nWe open at nine.\n")
	embedChunksForStatus(t, rig, "support", 1)
	embedChunksForStatus(t, rig, "faq", 1)

	byScope := map[string]health.Finding{}
	for _, f := range embeddingFindings(t, rig) {
		byScope[f.Scope] = f
	}
	if f := byScope["support"]; f.Code != "partial" || f.Status != health.StatusDegraded || !strings.Contains(f.Detail, "1 of 2") {
		t.Errorf("support = %+v, want partial", f)
	}
	if f := byScope["faq"]; f.Code != "ok" || f.Status != health.StatusUp {
		t.Errorf("faq = %+v, want ready", f)
	}

	rig.cfg.Knowledge.Embedding.Model = "another-model"
	byScope = map[string]health.Finding{}
	for _, f := range embeddingFindings(t, rig) {
		byScope[f.Scope] = f
	}
	for _, scope := range []string{"support", "faq"} {
		if f := byScope[scope]; f.Code != "kb_reembed_required" || f.Status != health.StatusDegraded {
			t.Errorf("%s = %+v, want kb_reembed_required", scope, f)
		}
	}
}

func TestStatusReportsAnOptedInCollectionWithoutConfigurationAsUnavailable(t *testing.T) {
	rig := newKnowledgeStatusRig(t)
	rig.cfg.Knowledge = statusEmbeddingConfig()
	rig.add(t, "faq", "b.md", "# Hours\nWe open at nine.\n")
	embedChunksForStatus(t, rig, "faq", 1)
	rig.cfg.Knowledge = nil
	findings := embeddingFindings(t, rig)
	if len(findings) != 1 || findings[0].Code != "kb_embedding_unavailable" || findings[0].Scope != "faq" {
		t.Fatalf("findings = %+v", findings)
	}
}

func TestStatusToleratesADatabaseOlderThanTheEmbeddingSchema(t *testing.T) {
	rig := newKnowledgeStatusRig(t)
	rig.cfg.Knowledge = statusEmbeddingConfig()
	rig.add(t, "faq", "b.md", "# Hours\nWe open at nine.\n")
	for _, statement := range []string{`DROP TABLE kb_embedding`, `ALTER TABLE kb_collection DROP COLUMN embedding_fingerprint`} {
		if _, err := rig.db.ExecContext(t.Context(), statement); err != nil {
			t.Fatalf("downgrade %q: %v", statement, err)
		}
	}
	findings := embeddingFindings(t, rig)
	if len(findings) != 1 || findings[0].Code != "knowledge_lexical_only" || findings[0].Status != health.StatusUp {
		t.Fatalf("findings = %+v, want lexical only without a failure", findings)
	}
}
