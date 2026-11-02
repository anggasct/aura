package cli

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
	toolsbuiltin "github.com/anggasct/aura/internal/tools/builtin"
	"github.com/anggasct/aura/internal/workflow"
)

type workflowToolController struct {
	interpreter *workflow.Interpreter
	runs        *workflow.Store
	registry    *auraagent.Registry
	targets     []replyTargeter
}

func newWorkflowToolController(interpreter *workflow.Interpreter, runs *workflow.Store, registry *auraagent.Registry, targets ...replyTargeter) (*workflowToolController, error) {
	definitions := registry.Definitions()
	for i := range definitions {
		for _, id := range definitions[i].Workflows {
			if _, ok := interpreter.Spec(id); !ok {
				return nil, &config.Error{Code: config.ErrorCodePersonaInvalid, Detail: fmt.Sprintf("persona %q workflows: %q is not a loaded workflow definition", definitions[i].ID, id)}
			}
		}
	}
	return &workflowToolController{interpreter: interpreter, runs: runs, registry: registry, targets: targets}, nil
}

func (c *workflowToolController) permits(caller *toolsbuiltin.WorkflowCaller, id string) bool {
	if caller.PrincipalClass == principal.ClassOwner {
		return true
	}
	definition, ok := c.registry.Lookup(caller.AgentID)
	return ok && slices.Contains(definition.Workflows, id)
}

func (c *workflowToolController) List(_ context.Context, caller *toolsbuiltin.WorkflowCaller) ([]toolsbuiltin.WorkflowInfo, error) {
	var workflows []toolsbuiltin.WorkflowInfo
	for _, spec := range c.interpreter.Definitions() {
		if c.permits(caller, spec.ID) {
			workflows = append(workflows, toolsbuiltin.WorkflowInfo{ID: spec.ID, Version: spec.Version, Goal: spec.Goal})
		}
	}
	slices.SortFunc(workflows, func(a, b toolsbuiltin.WorkflowInfo) int { return cmp.Compare(a.ID, b.ID) })
	return workflows, nil
}

func (c *workflowToolController) Start(ctx context.Context, caller *toolsbuiltin.WorkflowCaller, id, objective string) (toolsbuiltin.WorkflowRunState, error) {
	if !c.permits(caller, id) {
		return toolsbuiltin.WorkflowRunState{}, toolsbuiltin.ErrWorkflowNotPermitted
	}
	if _, ok := c.interpreter.Spec(id); !ok {
		return toolsbuiltin.WorkflowRunState{}, fmt.Errorf("workflow %q is not loaded", id)
	}
	origin := &workflow.RunOrigin{
		PrincipalID: caller.PrincipalID, PrincipalClass: string(caller.PrincipalClass), AgentID: caller.AgentID, SessionID: caller.SessionID,
	}
	for _, targeter := range c.targets {
		if target, ok := targeter.ReplyTarget(caller.SessionID); ok {
			origin.ReplyAccount, origin.ReplyChannelID = target.Account, target.ChannelID
			break
		}
	}
	summary, err := c.interpreter.StartFor(ctx, id, &workflow.RunInput{Objective: strings.TrimSpace(objective)}, origin)
	if err != nil {
		return toolsbuiltin.WorkflowRunState{}, err
	}
	return toolsbuiltin.WorkflowRunState{RunID: summary.ID, WorkflowID: summary.DefinitionID, Status: summary.Status}, nil
}

func (c *workflowToolController) visibleRun(ctx context.Context, caller *toolsbuiltin.WorkflowCaller, runID string) (*workflow.RunSummary, error) {
	run, err := c.runs.Run(ctx, runID)
	if code, ok := workflow.CodeOf(err); ok && code == workflow.ErrorCodeRunNotFound {
		return nil, toolsbuiltin.ErrWorkflowRunNotFound
	}
	if err != nil {
		return nil, err
	}
	if caller.PrincipalClass != principal.ClassOwner && (run.Origin.PrincipalID == "" || run.Origin.PrincipalID != caller.PrincipalID) {
		return nil, toolsbuiltin.ErrWorkflowRunNotFound
	}
	return run, nil
}

func (c *workflowToolController) Status(ctx context.Context, caller *toolsbuiltin.WorkflowCaller, runID string) (toolsbuiltin.WorkflowRunState, error) {
	run, err := c.visibleRun(ctx, caller, runID)
	if err != nil {
		return toolsbuiltin.WorkflowRunState{}, err
	}
	steps, err := c.runs.Steps(ctx, run.ID)
	if err != nil {
		return toolsbuiltin.WorkflowRunState{}, err
	}
	state := toolsbuiltin.WorkflowRunState{RunID: run.ID, WorkflowID: run.DefinitionID, Status: run.Status, Steps: make([]toolsbuiltin.WorkflowStepState, 0, len(steps))}
	for _, step := range steps {
		state.Steps = append(state.Steps, toolsbuiltin.WorkflowStepState{ID: step.StepID, Status: step.Status})
	}
	return state, nil
}

func (c *workflowToolController) Cancel(ctx context.Context, caller *toolsbuiltin.WorkflowCaller, runID string) (toolsbuiltin.WorkflowRunState, error) {
	run, err := c.visibleRun(ctx, caller, runID)
	if err != nil {
		return toolsbuiltin.WorkflowRunState{}, err
	}
	if err := c.interpreter.Cancel(ctx, run.ID); err != nil {
		return toolsbuiltin.WorkflowRunState{}, err
	}
	status, err := c.interpreter.RunStatus(ctx, run.ID)
	if err != nil {
		return toolsbuiltin.WorkflowRunState{}, err
	}
	return toolsbuiltin.WorkflowRunState{RunID: run.ID, WorkflowID: run.DefinitionID, Status: status}, nil
}

func workflowRejection(err error) error {
	if code, ok := workflow.CodeOf(err); ok {
		return &toolsbuiltin.WorkflowRejection{Code: string(code), Detail: err.Error()}
	}
	return err
}

func (c *workflowToolController) Propose(ctx context.Context, caller *toolsbuiltin.WorkflowCaller, raw json.RawMessage) (toolsbuiltin.WorkflowProposal, error) {
	if caller.PrincipalClass != principal.ClassOwner {
		return toolsbuiltin.WorkflowProposal{}, toolsbuiltin.ErrWorkflowNotPermitted
	}
	var spec workflow.Spec
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return toolsbuiltin.WorkflowProposal{}, &toolsbuiltin.WorkflowRejection{Code: string(workflow.ErrorCodeSpecInvalid), Detail: "spec is not a workflow spec: " + err.Error()}
	}
	id := workflow.NewProposalID()
	normalized := workflow.NormalizeProposal(&spec, id)
	definition, _ := c.registry.Lookup(caller.AgentID)
	graph, err := c.interpreter.CompileProposal(normalized, definition.Tools)
	if err != nil {
		return toolsbuiltin.WorkflowProposal{}, workflowRejection(err)
	}
	digest, err := workflow.Digest(normalized)
	if err != nil {
		return toolsbuiltin.WorkflowProposal{}, workflowRejection(err)
	}
	if err := c.runs.CreateProposal(ctx, &workflow.Proposal{
		ID: id, SessionID: caller.SessionID, AgentID: caller.AgentID, PrincipalID: caller.PrincipalID,
		Spec: normalized, Digest: digest, CreatedAt: time.Now().UTC(),
	}); err != nil {
		return toolsbuiltin.WorkflowProposal{}, err
	}
	plan := workflow.RenderPlan(normalized, graph) + "\nApprove with: aura workflow proposal approve " + id
	return toolsbuiltin.WorkflowProposal{ProposalID: id, Digest: digest, Plan: plan}, nil
}

func (c *workflowToolController) SaveRoutine(ctx context.Context, caller *toolsbuiltin.WorkflowCaller, runID, name string) (toolsbuiltin.WorkflowRoutine, error) {
	if caller.PrincipalClass != principal.ClassOwner {
		return toolsbuiltin.WorkflowRoutine{}, toolsbuiltin.ErrWorkflowNotPermitted
	}
	routine, err := c.interpreter.SaveRoutine(ctx, runID, name)
	if code, ok := workflow.CodeOf(err); ok && code == workflow.ErrorCodeRunNotFound {
		return toolsbuiltin.WorkflowRoutine{}, toolsbuiltin.ErrWorkflowRunNotFound
	}
	if err != nil {
		return toolsbuiltin.WorkflowRoutine{}, workflowRejection(err)
	}
	return toolsbuiltin.WorkflowRoutine{ID: routine.ID, Version: routine.Version}, nil
}
