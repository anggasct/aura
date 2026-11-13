package app

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/modules/workflow"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/health"
)

const (
	workflowDefinitionsComponent       = "workflow_definitions"
	codeWorkflowRoutineQuarantined     = "workflow_routine_quarantined"
	remediationFixWorkflowDefinitions  = "fix or remove the quarantined routines, then restart aura server"
	workflowDefinitionsHealthyDetail   = "every saved workflow routine compiles"
	workflowDefinitionsQuarantineStart = "saved workflow routines do not compile and are not runnable: "
)

type workflowDefinitionsHealth struct {
	quarantined []string
	personas    map[string][]string
}

func quarantinedIDs(quarantined []workflow.Quarantined) []string {
	ids := make([]string, 0, len(quarantined))
	for i := range quarantined {
		ids = append(ids, quarantined[i].ID)
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

func workflowDefinitionsCheck(cfg *config.Config, registry *auraagent.Registry, quarantined []workflow.Quarantined) health.RegisteredCheck {
	ids := quarantinedIDs(quarantined)
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
	return health.RegisteredCheck{
		ID:          workflowDefinitionsComponent,
		Checker:     &workflowDefinitionsHealth{quarantined: ids, personas: personas},
		Timeout:     time.Duration(cfg.Health.CheckTimeout),
		Remediation: remediationFixWorkflowDefinitions,
	}
}

func (h *workflowDefinitionsHealth) Check(context.Context) []health.Finding {
	if len(h.quarantined) == 0 {
		return []health.Finding{{Component: workflowDefinitionsComponent, Code: "ok", Status: health.StatusUp, Detail: workflowDefinitionsHealthyDetail}}
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
		Component: workflowDefinitionsComponent, Code: codeWorkflowRoutineQuarantined, Status: health.StatusDegraded,
		Detail: workflowDefinitionsQuarantineStart + strings.Join(parts, "; "), Remediation: remediationFixWorkflowDefinitions,
	}}
}
