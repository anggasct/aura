package memory

import (
	"context"
	"fmt"
	"time"

	"github.com/anggasct/aura/internal/platform/health"
)

const (
	ComponentMemoryEmbedding = "memory_embedding"
	remediationMemoryEmbed   = "check knowledge.embedding configuration and credentials; background backfill re-embeds pending documents automatically"
)

type memoryEmbeddingHealth struct {
	store     VectorStore
	settings  *EmbeddingSettings
	available bool
}

func (h *memoryEmbeddingHealth) Check(ctx context.Context) []health.Finding {
	now := time.Now().UTC()
	if h == nil || h.store == nil || h.settings == nil || !h.available {
		return []health.Finding{{
			Component:   ComponentMemoryEmbedding,
			Code:        "memory_embedding_unavailable",
			Status:      health.StatusDegraded,
			Severity:    health.SeverityWarning,
			Detail:      "memory embedding is enabled but the embedding provider or credential is unavailable; recall runs in lexical mode",
			Remediation: remediationMemoryEmbed,
			CheckedAt:   now,
		}}
	}
	cov, err := h.store.EmbeddingCoverage(ctx, h.settings.Fingerprint, h.settings.PublicQueries, now)
	if err != nil {
		return []health.Finding{{
			Component:   ComponentMemoryEmbedding,
			Code:        "memory_embedding_unknown",
			Status:      health.StatusUnknown,
			Detail:      "memory embedding coverage is unavailable",
			Remediation: remediationMemoryEmbed,
			CheckedAt:   now,
		}}
	}
	if cov.Stale > 0 {
		return []health.Finding{{
			Component:   ComponentMemoryEmbedding,
			Code:        "reembed_required",
			Status:      health.StatusDegraded,
			Severity:    health.SeverityWarning,
			Detail:      fmt.Sprintf("memory embeddings cover %d of %d documents (%d vectors belong to another model fingerprint and await re-embedding)", cov.Embedded, cov.Total, cov.Stale),
			Remediation: remediationMemoryEmbed,
			CheckedAt:   now,
		}}
	}
	if cov.Embedded < cov.Total {
		return []health.Finding{{
			Component:   ComponentMemoryEmbedding,
			Code:        "coverage_pending",
			Status:      health.StatusDegraded,
			Severity:    health.SeverityWarning,
			Detail:      fmt.Sprintf("memory embeddings cover %d of %d documents; pending or screened documents await the background backfill", cov.Embedded, cov.Total),
			Remediation: remediationMemoryEmbed,
			CheckedAt:   now,
		}}
	}
	return []health.Finding{{
		Component:   ComponentMemoryEmbedding,
		Code:        "ok",
		Status:      health.StatusUp,
		Detail:      fmt.Sprintf("memory embeddings cover %d of %d documents", cov.Embedded, cov.Total),
		Remediation: health.RemediationNone,
		CheckedAt:   now,
	}}
}

func (m *Module) HealthChecks() []health.RegisteredCheck {
	if m == nil || !m.cfg.EmbeddingEnabled {
		return nil
	}
	return []health.RegisteredCheck{{
		ID: ComponentMemoryEmbedding,
		Checker: &memoryEmbeddingHealth{
			store:     m.store,
			settings:  m.embedding,
			available: m.service != nil && m.service.EmbeddingActive(),
		},
		Timeout:     m.checkTimeout,
		Remediation: remediationMemoryEmbed,
	}}
}
