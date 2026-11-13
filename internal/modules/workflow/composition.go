package workflow

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/anggasct/aura/internal/platform/durable"
)

func (i *Interpreter) createChildRun(ctx context.Context, workflowID string, input *RunInput, parentRunID string, origin *RunOrigin) (*RunSummary, error) {
	compiled, ok := i.specs.get(workflowID)
	if !ok {
		return nil, codedError(ErrorCodeDefinitionNotFound, "definition "+workflowID+" is not loaded")
	}
	return i.store.createRun(ctx, compiled.spec, input, origin, newID("dur"), parentRunID)
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
	child, err := e.interpreter.createChildRun(ctx, *step.Executor.WorkflowID, e.input, e.runID, &e.origin)
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
		child, err := e.interpreter.createChildRun(actionCtx, *step.Executor.WorkflowID, e.input, e.runID, &e.origin)
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
