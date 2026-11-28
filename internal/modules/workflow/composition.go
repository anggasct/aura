package workflow

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/anggasct/aura/internal/platform/durable"
)

func (i *Interpreter) createChildRun(ctx context.Context, workflowID string, input *RunInput, parentRunID string, origin *RunOrigin) (*RunSummary, error) {
	compiled, ok := i.specs.get(workflowID)
	if !ok {
		return nil, codedError(ErrorCodeDefinitionNotFound, "definition "+workflowID+" is not loaded")
	}
	approval, err := i.requireSpecApproval(ctx, compiled.spec)
	if err != nil {
		return nil, err
	}
	claim, err := proposalClaimFor(approval, "")
	if err != nil {
		return nil, err
	}
	runOrigin, err := originUnderApproval(origin, approval)
	if err != nil {
		return nil, err
	}
	if err := i.admitRun(ctx, compiled.spec, runOrigin); err != nil {
		return nil, err
	}
	if err := i.checkScheduledChild(ctx, parentRunID, compiled.spec); err != nil {
		return nil, err
	}
	return i.store.createRun(ctx, &runCreate{Spec: compiled.spec, Input: input, Origin: runOrigin, DurableKey: newID("dur"), ParentRunID: parentRunID, ClaimProposalID: claim})
}

func (i *Interpreter) driveInline(ctx context.Context, inv durable.Invocation, runID string, inherited *runBudget) (string, error) {
	summary, err := i.store.Run(ctx, runID)
	if err != nil {
		return "", err
	}
	compiled, ok := i.compiledFor(ctx, summary.DefinitionID, summary.DefinitionVersion)
	if !ok {
		return "", codedError(ErrorCodeDefinitionNotFound, "definition "+summary.DefinitionID+" is not loaded")
	}
	digest, err := Digest(compiled.spec)
	if err != nil {
		return "", err
	}
	input, err := i.store.RunInputFor(ctx, runID)
	if err != nil {
		return "", err
	}
	if err := i.store.SetRunStatus(ctx, runID, RunRunning); err != nil {
		return "", err
	}
	execution := &stepExecution{
		interpreter:      i,
		invocation:       inv,
		spec:             compiled.spec,
		graph:            compiled.graph,
		runID:            runID,
		origin:           summary.Origin,
		definitionDigest: digest,
		input:            input,
		awaiting:         map[string]bool{},
		budget:           i.budgetFor(compiled.spec, inherited),
	}
	if err := execution.run(ctx); err != nil {
		return RunCancelled, err
	}
	return i.store.runStatus(context.WithoutCancel(ctx), runID)
}

func childUpdate(childID, status string, err error) *stepUpdate {
	output := json.RawMessage(`{"run_id":` + strconv.Quote(childID) + `,"status":` + strconv.Quote(status) + `}`)
	if err == nil && status == RunSucceeded {
		return &stepUpdate{Status: StepSucceeded, Output: output, EndedAt: nowPtr()}
	}
	detail := "child run " + childID + " ended " + status
	if err != nil {
		detail += ": " + err.Error()
	}
	return &stepUpdate{Status: StepFailed, Output: output, ErrorCode: string(ErrorCodeChildFailed), Detail: detail, EndedAt: nowPtr()}
}

func (e *stepExecution) runWorkflowStep(ctx context.Context, step *StepSpec) *stepUpdate {
	origin := e.originFor(step.ID)
	child, err := e.interpreter.createChildRun(ctx, *step.Executor.WorkflowID, e.input, e.runID, &origin)
	if err != nil {
		return &stepUpdate{Status: StepFailed, ErrorCode: string(ErrorCodeChildFailed), Detail: err.Error(), EndedAt: nowPtr()}
	}
	status, err := e.interpreter.driveInline(ctx, e.invocation, child.ID, e.budget)
	return childUpdate(child.ID, status, err)
}

func (e *stepExecution) runWorkflowStepSerial(ctx context.Context, step *StepSpec, attempt int) (*stepUpdate, bool) {
	if err := e.terminal(ctx, step.ID, &stepUpdate{Status: StepRunning, Attempt: attempt}); err != nil {
		e.failRun(ctx, step.ID, err)
		return nil, true
	}
	key := "run/" + e.runID + "/step/" + step.ID + "/attempt/" + strconv.Itoa(attempt) + "/child"
	raw, err := e.invocation.RunAction(ctx, key, func(actionCtx context.Context) ([]byte, error) {
		origin := e.originFor(step.ID)
		child, err := e.interpreter.createChildRun(actionCtx, *step.Executor.WorkflowID, e.input, e.runID, &origin)
		if err != nil {
			return nil, err
		}
		return []byte(child.ID), nil
	})
	if err != nil {
		e.failRun(ctx, step.ID, err)
		return nil, true
	}
	status, driveErr := e.interpreter.driveSerial(ctx, e.invocation, string(raw), e.budget)
	if ctx.Err() != nil {
		return nil, true
	}
	update := childUpdate(string(raw), status, driveErr)
	update.Attempt = attempt + 1
	return update, false
}

const maxRunAncestry = 64

func (i *Interpreter) childLookup() ChildLookup {
	return func(id string) (*Spec, error) {
		compiled, ok := i.specs.get(id)
		if !ok {
			return nil, codedError(ErrorCodeDefinitionNotFound, "definition "+id+" is not loaded")
		}
		return compiled.spec, nil
	}
}

func (i *Interpreter) scheduledRoot(ctx context.Context, runID string) (bool, error) {
	for range maxRunAncestry {
		run, err := i.store.Run(ctx, runID)
		if err != nil {
			return false, err
		}
		if run.ParentRunID == "" {
			return strings.HasPrefix(run.DurableKey, ScheduledKeyPrefix), nil
		}
		runID = run.ParentRunID
	}
	return false, codedError(ErrorCodeCompositionInvalid, "the run has too many ancestors")
}

func (i *Interpreter) checkScheduledChild(ctx context.Context, parentRunID string, child *Spec) error {
	scheduled, err := i.scheduledRoot(ctx, parentRunID)
	if err != nil {
		return err
	}
	if !scheduled {
		return nil
	}
	parent, err := i.store.Run(ctx, parentRunID)
	if err != nil {
		return err
	}
	parentSpec, ok := i.compiledFor(ctx, parent.DefinitionID, parent.DefinitionVersion)
	if !ok {
		return codedError(ErrorCodeDefinitionNotFound, "definition "+parent.DefinitionID+" is not loaded")
	}
	deps := i.ValidationDeps()
	if err := CheckUnattendedSinks(parentSpec.spec, deps, i.childLookup()); err != nil {
		return err
	}
	return CheckUnattendedSinks(child, deps, i.childLookup())
}
