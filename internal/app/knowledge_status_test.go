package app

import (
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/platform/health"
)

func knowledgeFindings(t *testing.T, rig *kbRetrievalRig) []health.Finding {
	t.Helper()
	registry, err := buildHealthRegistry(rig.cfg, nil, func() (bool, string) { return true, "ok" }, func() (health.ProcessStatus, bool) { return health.ProcessStatus{}, false })
	if err != nil {
		t.Fatalf("buildHealthRegistry: %v", err)
	}
	var found []health.Finding
	all := registry.Evaluate(t.Context())
	for i := range all {
		if all[i].Component == health.ComponentKnowledge {
			found = append(found, all[i])
		}
	}
	return found
}

func newKnowledgeStatusRig(t *testing.T) *kbRetrievalRig {
	t.Helper()
	rig := newKnowledgeTurnRig(t)
	return rig.kbRetrievalRig
}

func TestStatusReportsUnknownCollectionsOfAConfiguredPersona(t *testing.T) {
	rig := newKnowledgeStatusRig(t)
	rig.add(t, "support", "a.md", "Refunds are issued within thirty days.")
	findings := knowledgeFindings(t, rig)
	byCode := map[string][]health.Finding{}
	for _, f := range findings {
		byCode[f.Code] = append(byCode[f.Code], f)
	}
	if len(byCode["knowledge_collection_unknown"]) != 3 {
		t.Fatalf("findings = %+v, want hr-docs/faq unknown for owner, faq for trusted and public", findings)
	}
	if len(byCode["knowledge_scope_oversized"]) != 0 {
		t.Errorf("a small scope was reported oversized: %+v", byCode["knowledge_scope_oversized"])
	}
}

func TestStatusReportsAScopeAboveTheSupportedSize(t *testing.T) {
	rig := newKnowledgeStatusRig(t)
	rig.cfg.Health.CheckTimeout = config.Duration(30 * time.Second)
	if _, err := rig.db.ExecContext(t.Context(), `DROP TRIGGER kb_chunk_fts_insert`); err != nil {
		t.Fatalf("drop index trigger: %v", err)
	}
	content := `'general information item ' || i`
	seedBulkKBScope(t, rig.db, "support", knowledge.MaxChunksPerCollection-1, content)
	seedBulkKBScope(t, rig.db, "hr-docs", knowledge.MaxChunksPerCollection-1, content)
	seedBulkKBScope(t, rig.db, "faq", 2000, content)

	findings := knowledgeFindings(t, rig)
	if len(findings) != 1 || findings[0].Code != "knowledge_scope_oversized" || findings[0].Scope != "lobby/owner" || findings[0].Status != health.StatusDegraded {
		t.Fatalf("findings = %+v, want only the owner scope of lobby reported", findings)
	}
}

func TestStatusStaysQuietWhenNoPersonaListsKnowledge(t *testing.T) {
	rig := newKnowledgeStatusRig(t)
	rig.cfg.Agents.Definitions = nil
	if findings := knowledgeFindings(t, rig); len(findings) != 0 {
		t.Fatalf("findings = %+v, want no knowledge check without configured collections", findings)
	}
}

func TestStatusToleratesADatabaseOlderThanTheKnowledgeSchema(t *testing.T) {
	rig := newKnowledgeStatusRig(t)
	for _, drop := range []string{"kb_chunk_fts", "kb_chunk", "kb_source", "kb_collection"} {
		if _, err := rig.db.ExecContext(t.Context(), "DROP TABLE "+drop); err != nil {
			t.Fatalf("drop %s: %v", drop, err)
		}
	}
	findings := knowledgeFindings(t, rig)
	if len(findings) != 1 || findings[0].Code != "knowledge_index_missing" {
		t.Fatalf("findings = %+v, want the missing-index finding and no failure", findings)
	}
}
