package workflow

import (
	"context"
	"slices"
	"strings"
	"time"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/health"
)

const (
	definitionsComponent       = "workflow_definitions"
	codeRoutineQuarantined     = "workflow_routine_quarantined"
	remediationFixDefinitions  = "fix or remove the quarantined routines, then restart aura server"
	definitionsHealthyDetail   = "every saved workflow routine compiles"
	definitionsQuarantineStart = "saved workflow routines do not compile and are not runnable: "
)

type definitionsHealth struct {
	quarantined []string
	personas    map[string][]string
}

func QuarantinedIDs(quarantined []Quarantined) []string {
	ids := make([]string, 0, len(quarantined))
	for i := range quarantined {
		ids = append(ids, quarantined[i].ID)
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

func HealthChecks(registry *auraagent.Registry, quarantined []Quarantined, timeout time.Duration) []health.RegisteredCheck {
	ids := QuarantinedIDs(quarantined)
	personas := make(map[string][]string)
	if registry != nil {
		definitions := registry.Definitions()
		for i := range definitions {
			for _, id := range definitions[i].Workflows {
				if slices.Contains(ids, id) {
					personas[id] = append(personas[id], definitions[i].ID)
				}
			}
		}
	}
	return []health.RegisteredCheck{{
		ID:          definitionsComponent,
		Checker:     &definitionsHealth{quarantined: ids, personas: personas},
		Timeout:     timeout,
		Remediation: remediationFixDefinitions,
	}}
}

func (h *definitionsHealth) Check(context.Context) []health.Finding {
	if len(h.quarantined) == 0 {
		return []health.Finding{{
			Component: definitionsComponent,
			Code:      "ok",
			Status:    health.StatusUp,
			Detail:    definitionsHealthyDetail,
		}}
	}
	parts := make([]string, 0, len(h.quarantined))
	for _, id := range h.quarantined {
		part := id
		if listed := h.personas[id]; len(listed) > 0 {
			part += " (listed by persona " + strings.Join(listed, ", ") + ")"
		}
		parts = append(parts, part)
	}
	return []health.Finding{{
		Component:   definitionsComponent,
		Code:        codeRoutineQuarantined,
		Status:      health.StatusDegraded,
		Detail:      definitionsQuarantineStart + strings.Join(parts, "; "),
		Remediation: remediationFixDefinitions,
	}}
}
