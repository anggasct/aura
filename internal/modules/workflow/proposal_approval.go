package workflow

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/principal"
)

const planNotStoredLine = "plan: not stored; this proposal predates plan storage; propose it again"

type planCheck func(proposal *Proposal, graph *Graph) error

func ApproveRenderedProposal(ctx context.Context, interpreter *Interpreter, runs *Store, id string, render ToolCallText, now time.Time) (string, error) {
	return approveOnStoredPlan(ctx, interpreter, runs, id, now, func(proposal *Proposal, graph *Graph) error {
		if render == nil {
			return planUnshowable("no tool display is available to show the plan again")
		}
		plan, err := RenderPlan(proposal.Spec, graph, render)
		if err != nil {
			return err
		}
		if plan != proposal.PlanText {
			return planUnshowable("the plan changed since it was proposed; propose it again")
		}
		return nil
	})
}

func ApproveStoredProposal(ctx context.Context, interpreter *Interpreter, runs *Store, id string, secrets []string, now time.Time) (string, error) {
	return approveOnStoredPlan(ctx, interpreter, runs, id, now, func(proposal *Proposal, _ *Graph) error {
		if holdsSecret(proposal.PlanText, secrets) || stepArgumentsHoldSecret(proposal.Spec, secrets) {
			return planUnshowable("the plan holds a configured secret value; propose it again without it")
		}
		return nil
	})
}

func planUnshowable(detail string) error {
	return codedError(ErrorCodePlanUnshowable, detail)
}

func holdsSecret(text string, secrets []string) bool {
	for _, secret := range secrets {
		if secret != "" && strings.Contains(text, secret) {
			return true
		}
	}
	return false
}

func valueHoldsSecret(value any, secrets []string) bool {
	switch typed := value.(type) {
	case string:
		return holdsSecret(typed, secrets)
	case []any:
		return slices.ContainsFunc(typed, func(item any) bool { return valueHoldsSecret(item, secrets) })
	case map[string]any:
		for key, item := range typed {
			if holdsSecret(key, secrets) || valueHoldsSecret(item, secrets) {
				return true
			}
		}
	}
	return false
}

func stepArgumentsHoldSecret(spec *Spec, secrets []string) bool {
	for index := range spec.Steps {
		executor := &spec.Steps[index].Executor
		if executor.Kind != KindTool {
			continue
		}
		if holdsSecret(string(executor.ToolArgs), secrets) {
			return true
		}
		var decoded any
		if json.Unmarshal(executor.ToolArgs, &decoded) == nil && valueHoldsSecret(decoded, secrets) {
			return true
		}
	}
	return false
}

func approveOnStoredPlan(ctx context.Context, interpreter *Interpreter, runs *Store, id string, now time.Time, check planCheck) (string, error) {
	if interpreter == nil || runs == nil {
		return "", codedError(ErrorCodeInvalidArgument, "proposal approval requires an interpreter and a workflow store")
	}
	proposal, err := runs.Proposal(ctx, id, now)
	if err != nil {
		return "", err
	}
	if proposal.Status != ProposalProposed {
		return "", codedError(ErrorCodeProposalDecided, "proposal "+id+" is "+proposal.Status)
	}
	digest, err := Digest(proposal.Spec)
	if err != nil {
		return "", err
	}
	if digest != proposal.Digest {
		return "", codedError(ErrorCodeDigestMismatch, "proposal "+id+" no longer matches the digest it was proposed with")
	}
	if proposal.PlanText == "" {
		return "", planUnshowable("proposal " + id + " has no stored plan; propose it again")
	}
	graph, err := interpreter.CompileProposal(proposal.Spec, proposal.AgentID)
	if err != nil {
		return "", err
	}
	if err := check(proposal, graph); err != nil {
		return "", err
	}
	if err := interpreter.LoadAll(ctx, []*Spec{proposal.Spec}, interpreter.ValidationDeps()); err != nil {
		return "", err
	}
	if err := runs.ApproveProposal(ctx, id, &Approval{
		DefinitionID: proposal.Spec.ID, DefinitionVersion: proposal.Spec.Version, Digest: digest,
		AgentID: proposal.AgentID, ApproverID: principal.OwnerID, Tainted: proposal.Tainted, ApprovedAt: now,
	}); err != nil {
		return "", err
	}
	summary, err := interpreter.StartApproved(ctx, id, &RunInput{}, &RunOrigin{
		PrincipalID: proposal.PrincipalID, PrincipalClass: string(principal.ClassOwner), AgentID: proposal.AgentID, SessionID: proposal.SessionID, Reply: proposal.Reply, Tainted: proposal.Tainted,
	})
	if err != nil {
		return "", err
	}
	return summary.ID, nil
}
