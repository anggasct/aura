package profile

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/anggasct/aura/internal/platform/health"
)

const (
	ComponentProfileEmbedding = "profile_embedding"

	extractMaxOutputTokens = 1024
	failingThreshold       = 3

	remediationRouteProfiling  = "set models.routing.profiling to a configured route (for example primary) or define models.definitions.auxiliary, then restart"
	remediationReviewProfiling = "check the profiling model route and provider health; extraction resumes on the next completed job"
	remediationProfileEmbed    = "check knowledge.embedding configuration and credentials; background backfill re-embeds verified active profile facts automatically"
)

type profileHealth struct {
	unavailable atomic.Bool
	failures    atomic.Int64
}

func (h *profileHealth) observe(observation *Observation) {
	if observation == nil || observation.Kind != ObserveExtraction {
		return
	}
	switch observation.Result {
	case ResultCompleted:
		h.failures.Store(0)
	case ResultFailed, ResultParseFailed:
		h.failures.Add(1)
	}
}

func (h *profileHealth) Check(context.Context) []health.Finding {
	if h.unavailable.Load() {
		return []health.Finding{{
			Component:   "profile",
			Code:        "profile_extractor_unavailable",
			Status:      health.StatusDegraded,
			Severity:    health.SeverityWarning,
			Detail:      "the profiling model route is not configured; the bot does not learn new preferences, verified facts are still served",
			Remediation: remediationRouteProfiling,
		}}
	}
	if h.failures.Load() >= failingThreshold {
		return []health.Finding{{
			Component:   "profile",
			Code:        "profile_extraction_failing",
			Status:      health.StatusDegraded,
			Severity:    health.SeverityWarning,
			Detail:      "recent profile extraction jobs failed; turns are unaffected",
			Remediation: remediationReviewProfiling,
		}}
	}
	return nil
}

type profileEmbeddingHealth struct {
	store     *SQLiteStore
	settings  *EmbeddingSettings
	available bool
}

func (h *profileEmbeddingHealth) Check(ctx context.Context) []health.Finding {
	now := time.Now().UTC()
	if h == nil || h.store == nil || h.settings == nil || !h.available {
		return []health.Finding{{
			Component:   ComponentProfileEmbedding,
			Code:        "profile_embedding_unavailable",
			Status:      health.StatusDegraded,
			Severity:    health.SeverityWarning,
			Detail:      "profile embedding is enabled but the embedding provider or credential is unavailable; profile search runs in lexical mode",
			Remediation: remediationProfileEmbed,
			CheckedAt:   now,
		}}
	}
	cov, err := h.store.EmbeddingCoverage(ctx, h.settings.Fingerprint, now)
	if err != nil {
		return []health.Finding{{
			Component:   ComponentProfileEmbedding,
			Code:        "profile_embedding_unknown",
			Status:      health.StatusUnknown,
			Detail:      "profile embedding coverage is unavailable",
			Remediation: remediationProfileEmbed,
			CheckedAt:   now,
		}}
	}
	if cov.Stale > 0 {
		return []health.Finding{{
			Component:   ComponentProfileEmbedding,
			Code:        "reembed_required",
			Status:      health.StatusDegraded,
			Severity:    health.SeverityWarning,
			Detail:      fmt.Sprintf("profile embeddings cover %d of %d verified active facts (%d vectors belong to another model fingerprint and await re-embedding)", cov.Embedded, cov.Total, cov.Stale),
			Remediation: remediationProfileEmbed,
			CheckedAt:   now,
		}}
	}
	return []health.Finding{{
		Component:   ComponentProfileEmbedding,
		Code:        "ok",
		Status:      health.StatusUp,
		Detail:      fmt.Sprintf("profile embeddings cover %d of %d verified active facts", cov.Embedded, cov.Total),
		Remediation: health.RemediationNone,
		CheckedAt:   now,
	}}
}

func (m *Module) HealthChecks() []health.RegisteredCheck {
	if m == nil || m.health == nil {
		return nil
	}
	checks := []health.RegisteredCheck{{
		ID:          "profile",
		Checker:     m.health,
		Timeout:     m.checkTimeout,
		Remediation: remediationReviewProfiling,
	}}
	if m.cfg.EmbeddingEnabled {
		checks = append(checks, health.RegisteredCheck{
			ID: ComponentProfileEmbedding,
			Checker: &profileEmbeddingHealth{
				store:     m.store,
				settings:  m.embedding,
				available: m.store != nil && m.store.EmbeddingActive(),
			},
			Timeout:     m.checkTimeout,
			Remediation: remediationProfileEmbed,
		})
	}
	return checks
}
