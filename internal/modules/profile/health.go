package profile

import (
	"context"
	"sync/atomic"

	"github.com/anggasct/aura/internal/platform/health"
)

const (
	extractMaxOutputTokens = 1024
	failingThreshold       = 3

	remediationRouteProfiling  = "set models.routing.profiling to a configured route (for example primary) or define models.definitions.auxiliary, then restart"
	remediationReviewProfiling = "check the profiling model route and provider health; extraction resumes on the next completed job"
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

func (m *Module) HealthChecks() []health.RegisteredCheck {
	if m == nil || m.health == nil {
		return nil
	}
	return []health.RegisteredCheck{{
		ID:          "profile",
		Checker:     m.health,
		Timeout:     m.checkTimeout,
		Remediation: remediationReviewProfiling,
	}}
}
