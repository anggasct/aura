package workflow

import (
	"cmp"
	"context"
	"fmt"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/principal"
)

func checkPersona(deps *ValidationDeps, agentID string, class principal.Class) error {
	if deps.Agents == nil {
		return codedError(ErrorCodePersonaMissing, fmt.Sprintf("persona %q cannot be checked: no persona registry is configured", agentID))
	}
	definition, err := deps.Agents.Resolve(nil, &agentID)
	if err != nil {
		return codedError(ErrorCodePersonaMissing, fmt.Sprintf("persona %q is not configured", agentID))
	}
	if !definition.Admits(class) {
		return codedError(ErrorCodeNotPermitted, fmt.Sprintf("persona %q does not serve %s principals", agentID, class))
	}
	return nil
}

func configuredAgent(spec *Spec, approval *Approval) string {
	if approval.Kind == ApprovalInstalled {
		return cmp.Or(spec.RunAs, auraagent.DefaultID)
	}
	return approval.AgentID
}

func (i *Interpreter) admitRun(ctx context.Context, spec *Spec, origin *RunOrigin) error {
	if !origin.Complete() {
		return codedError(ErrorCodeOriginIncomplete, "a run needs a principal, a known principal class and an agent")
	}
	deps := i.ValidationDeps()
	class := principal.ParseClass(origin.PrincipalClass)
	if err := checkPersona(deps, origin.AgentID, class); err != nil {
		return err
	}
	seen := map[string]bool{}
	var walk func(current *Spec) error
	walk = func(current *Spec) error {
		if seen[current.ID] {
			return nil
		}
		seen[current.ID] = true
		if err := CheckCeiling(current, origin.AgentID, class, deps); err != nil {
			return fmt.Errorf("definition %s: %w", current.ID, err)
		}
		for index := range current.Steps {
			step := &current.Steps[index]
			if step.Executor.Kind != KindWorkflow || step.Executor.WorkflowID == nil {
				continue
			}
			if child, ok := i.Spec(*step.Executor.WorkflowID); ok {
				if err := i.refuseSpentChild(ctx, child); err != nil {
					return err
				}
				if err := walk(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(spec)
}

func (i *Interpreter) refuseSpentChild(ctx context.Context, child *Spec) error {
	approval, err := i.requireSpecApproval(ctx, child)
	if code, ok := CodeOf(err); ok && code == ErrorCodeApprovalMissing {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := proposalClaimFor(approval, ""); err != nil {
		return fmt.Errorf("definition %s: %w", child.ID, err)
	}
	return nil
}
