package personaeval

import (
	"context"
	"time"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/health"
)

const (
	Component            = "persona_eval"
	CodeRegressed        = "persona_eval_regressed"
	CodeLookupFailed     = "persona_eval_lookup_failed"
	RemediationRegressed = "re-run `aura persona eval <id> --behavior` after reviewing the persona; set evals.behavior.enforce_on_regression to stop serving non-owners on regression"
)

type HealthChecker struct {
	registry *auraagent.Registry
	results  Store
}

func NewHealthChecker(registry *auraagent.Registry, results Store) (*HealthChecker, error) {
	if registry == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "persona eval health checker registry must not be nil")
	}
	if results == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "persona eval health checker store must not be nil")
	}
	return &HealthChecker{registry: registry, results: results}, nil
}

func HealthChecks(registry *auraagent.Registry, results Store, timeout time.Duration) ([]health.RegisteredCheck, error) {
	checker, err := NewHealthChecker(registry, results)
	if err != nil {
		return nil, err
	}
	return []health.RegisteredCheck{{
		ID:          Component,
		Checker:     checker,
		Timeout:     timeout,
		Remediation: RemediationRegressed,
	}}, nil
}

func (h *HealthChecker) Check(ctx context.Context) []health.Finding {
	if h == nil || h.registry == nil || h.results == nil {
		return nil
	}
	var findings []health.Finding
	definitions := h.registry.Definitions()
	for i := range definitions {
		definition := &definitions[i]
		if definition.EvalBehavior == nil {
			continue
		}
		regressed, err := h.regressed(ctx, definition)
		if err != nil {
			findings = append(findings, health.Finding{
				Component: Component, Scope: definition.ID, Code: CodeLookupFailed,
				Status: health.StatusUnknown, Detail: "persona evaluation results could not be read",
			})
			continue
		}
		if regressed {
			findings = append(findings, health.Finding{
				Component: Component, Scope: definition.ID, Code: CodeRegressed,
				Status: health.StatusDegraded, Severity: health.SeverityWarning,
				Detail:      "persona " + definition.ID + " failed its latest behavior evaluation after passing it for the same version",
				Remediation: RemediationRegressed,
			})
		}
	}
	return findings
}

func (h *HealthChecker) regressed(ctx context.Context, definition *auraagent.Definition) (bool, error) {
	digest := definition.EvalDigest()
	latest, found, err := h.results.Latest(ctx, definition.ID, digest, LayerBehavior)
	if err != nil || !found || latest.Passed {
		return false, err
	}
	return h.results.EverPassed(ctx, definition.ID, digest, LayerBehavior)
}
