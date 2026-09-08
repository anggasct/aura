package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/anggasct/aura/internal/durable"
)

type journaledAttempt struct {
	Status         string          `json:"status"`
	Attempt        int             `json:"attempt"`
	StartedAt      *time.Time      `json:"started_at,omitempty"`
	EndedAt        *time.Time      `json:"ended_at,omitempty"`
	Output         json.RawMessage `json:"output,omitempty"`
	ArtifactDigest string          `json:"artifact_digest,omitempty"`
	ErrorCode      string          `json:"error_code,omitempty"`
	Detail         string          `json:"detail,omitempty"`
}

func journaledFromUpdate(update *stepUpdate) journaledAttempt {
	return journaledAttempt{
		Status:         update.Status,
		Attempt:        update.Attempt,
		StartedAt:      update.StartedAt,
		EndedAt:        update.EndedAt,
		Output:         update.Output,
		ArtifactDigest: update.ArtifactDigest,
		ErrorCode:      update.ErrorCode,
		Detail:         update.Detail,
	}
}

func updateFromJournaled(record *journaledAttempt) *stepUpdate {
	return &stepUpdate{
		Status:         record.Status,
		Attempt:        record.Attempt,
		StartedAt:      record.StartedAt,
		EndedAt:        record.EndedAt,
		Output:         []byte(record.Output),
		ArtifactDigest: record.ArtifactDigest,
		ErrorCode:      record.ErrorCode,
		Detail:         record.Detail,
	}
}

func (i *Interpreter) DriveSerial(ctx context.Context, inv durable.Invocation, runID string) (string, error) {
	if inv == nil {
		return "", codedError(ErrorCodeStepFailed, "serial drive requires an invocation")
	}
	summary, err := i.store.Run(ctx, runID)
	if err != nil {
		return "", err
	}
	compiled, ok := i.specs.get(summary.DefinitionID)
	if !ok {
		return "", codedError(ErrorCodeDefinitionNotFound, "definition "+summary.DefinitionID+" is not loaded")
	}
	input, err := i.store.RunInputFor(ctx, runID)
	if err != nil {
		return "", err
	}
	if err := i.store.SetRunStatus(ctx, runID, RunRunning); err != nil {
		return "", err
	}
	execution := &stepExecution{
		interpreter: i,
		invocation:  inv,
		spec:        compiled.spec,
		graph:       compiled.graph,
		runID:       runID,
		input:       input,
		outputs:     map[string]json.RawMessage{},
		statuses:    map[string]string{},
		awaiting:    map[string]bool{},
	}
	for _, stepID := range compiled.graph.Order {
		execution.driveStep(ctx, compiled.graph.ByStep[stepID])
		if ctx.Err() != nil {
			break
		}
	}
	persistCtx := context.WithoutCancel(ctx)
	if err := ctx.Err(); err != nil {
		if setErr := i.store.SetRunStatus(persistCtx, runID, RunCancelled); setErr != nil {
			return "", setErr
		}
		return "", err
	}
	terminal := RunSucceeded
	if execution.failedStep != "" {
		terminal = RunFailed
	}
	if err := i.store.SetRunStatus(persistCtx, runID, terminal); err != nil {
		return "", err
	}
	return terminal, nil
}

func (e *stepExecution) driveStep(ctx context.Context, step *StepSpec) {
	if e.isFailed() {
		return
	}
	for _, dependency := range step.DependsOn {
		if e.statusFor(dependency) == StepSkipped {
			if err := e.terminal(ctx, step.ID, &stepUpdate{Status: StepSkipped}); err != nil {
				e.failRun(ctx, step.ID, err)
			}
			return
		}
	}
	if step.Condition != nil {
		parsed, err := parseCondition(*step.Condition)
		if err != nil {
			e.failRun(ctx, step.ID, err)
			return
		}
		if !e.evaluate(parsed) {
			if err := e.terminal(ctx, step.ID, &stepUpdate{Status: StepSkipped}); err != nil {
				e.failRun(ctx, step.ID, err)
			}
			return
		}
	}

	attempt := 0
	for {
		update, ended := e.executeSerial(ctx, step, attempt)
		if ended {
			return
		}
		if update.ErrorCode == "" {
			if err := e.sinkOutput(ctx, update); err != nil {
				if terminalErr := e.terminal(ctx, step.ID, &stepUpdate{Status: StepFailed, Attempt: update.Attempt, ErrorCode: string(ErrorCodeStepFailed), Detail: err.Error(), EndedAt: nowPtr()}); terminalErr != nil {
					err = errors.Join(err, terminalErr)
				}
				e.failRun(ctx, step.ID, err)
				return
			}
			if err := e.terminal(ctx, step.ID, update); err != nil {
				e.failRun(ctx, step.ID, err)
				return
			}
			e.recordSuccess(step.ID, update.Output)
			return
		}
		if attempt >= step.Retry.Attempts {
			if err := e.terminal(ctx, step.ID, update); err != nil {
				e.failRun(ctx, step.ID, err)
				return
			}
			e.failRun(ctx, step.ID, &Error{Code: ErrorCode(update.ErrorCode), Detail: fmt.Sprintf("step %s exhausted %d attempts", step.ID, attempt+1)})
			return
		}
		if err := e.terminal(ctx, step.ID, &stepUpdate{Status: StepFailed, Attempt: attempt, ErrorCode: update.ErrorCode}); err != nil {
			e.failRun(ctx, step.ID, err)
			return
		}
		if err := e.invocation.Sleep(step.Retry.Backoff); err != nil {
			return
		}
		attempt++
	}
}

func (e *stepExecution) executeSerial(ctx context.Context, step *StepSpec, attempt int) (*stepUpdate, bool) {
	switch step.Executor.Kind {
	case KindAgent, KindTool:
		return e.executeActionSerial(ctx, step, attempt)
	case KindWait:
		return e.runWaitStepSerial(ctx, step, attempt), false
	case KindApproval:
		return e.runApprovalStepSerial(ctx, step, attempt), false
	default:
		return &stepUpdate{Status: StepFailed, ErrorCode: string(ErrorCodeExecutorInvalid), Detail: fmt.Sprintf("kind %q is not executable", step.Executor.Kind), EndedAt: nowPtr()}, false
	}
}

func (e *stepExecution) executeActionSerial(ctx context.Context, step *StepSpec, attempt int) (*stepUpdate, bool) {
	if err := e.terminal(ctx, step.ID, &stepUpdate{Status: StepRunning, Attempt: attempt}); err != nil {
		e.failRun(ctx, step.ID, err)
		return nil, true
	}
	attemptCtx, cancel := context.WithTimeout(ctx, step.Timeout)
	defer cancel()
	key := "run/" + e.runID + "/step/" + step.ID + "/attempt/" + strconv.Itoa(attempt)
	raw, err := e.invocation.RunAction(attemptCtx, key, func(actionCtx context.Context) ([]byte, error) {
		update := e.invokeExecutor(actionCtx, step, attempt)
		update.Attempt = attempt + 1
		return json.Marshal(journaledFromUpdate(update))
	})
	if err != nil {
		e.failRun(ctx, step.ID, err)
		return nil, true
	}
	var record journaledAttempt
	if err := json.Unmarshal(raw, &record); err != nil {
		e.failRun(ctx, step.ID, err)
		return nil, true
	}
	if attemptCtx.Err() != nil {
		return &stepUpdate{
			Status:    StepFailed,
			Attempt:   attempt + 1,
			EndedAt:   nowPtr(),
			ErrorCode: string(ErrorCodeStepTimeout),
		}, false
	}
	return updateFromJournaled(&record), false
}

func (e *stepExecution) bindWaitCorrelation(ctx context.Context, step *StepSpec, attempt int) (*stepUpdate, bool) {
	if step.Executor.ExternalRef == nil || step.Executor.Event == nil {
		return nil, false
	}
	output, ok := e.resolvedOutput(*step.Executor.ExternalRef)
	if !ok {
		return &stepUpdate{Status: StepFailed, Attempt: attempt + 1, ErrorCode: string(ErrorCodeExecutorInvalid), Detail: fmt.Sprintf("wait step %s binds no output from step %q", step.ID, *step.Executor.ExternalRef), EndedAt: nowPtr()}, true
	}
	var decoded struct {
		ExternalID string `json:"external_id"`
	}
	if err := json.Unmarshal(output, &decoded); err != nil || decoded.ExternalID == "" {
		return &stepUpdate{Status: StepFailed, Attempt: attempt + 1, ErrorCode: string(ErrorCodeExecutorInvalid), Detail: fmt.Sprintf("wait step %s needs an external_id in step %q output", step.ID, *step.Executor.ExternalRef), EndedAt: nowPtr()}, true
	}
	source := "github"
	if step.Executor.Source != nil {
		source = *step.Executor.Source
	}
	err := e.interpreter.store.BindCorrelation(ctx, &Correlation{
		Source:     source,
		EventType:  *step.Executor.Event,
		ExternalID: decoded.ExternalID,
		RunID:      e.runID,
		SignalName: "wait." + step.ID,
		DedupeKey:  "",
	})
	if err != nil {
		if code, ok := CodeOf(err); ok && code == ErrorCodeCorrelationConflict {
			return nil, false
		}
		return &stepUpdate{Status: StepFailed, Attempt: attempt + 1, ErrorCode: string(ErrorCodeStepFailed), Detail: err.Error(), EndedAt: nowPtr()}, true
	}
	return nil, false
}

func (e *stepExecution) runWaitStepSerial(ctx context.Context, step *StepSpec, attempt int) *stepUpdate {
	if update, failed := e.bindWaitCorrelation(ctx, step, attempt); failed {
		return update
	}
	e.suspendRun(ctx, step.ID)
	payload, timedOut, ok := e.invocation.Wait(ctx, "wait."+step.ID, step.Timeout)
	e.resumeRun(ctx, step.ID)
	switch {
	case !ok:
		return &stepUpdate{Status: StepFailed, Attempt: attempt + 1, ErrorCode: "", EndedAt: nowPtr(), Detail: errWaitCancelled.Error()}
	case timedOut:
		return &stepUpdate{
			Status:    StepFailed,
			Attempt:   attempt + 1,
			EndedAt:   nowPtr(),
			ErrorCode: string(ErrorCodeStepTimeout),
		}
	default:
		return &stepUpdate{Status: StepSucceeded, Attempt: attempt + 1, EndedAt: nowPtr(), Output: payload}
	}
}

func (e *stepExecution) runApprovalStepSerial(ctx context.Context, step *StepSpec, attempt int) *stepUpdate {
	if requester := e.interpreter.options.Approvals; requester != nil {
		if _, err := e.invocation.RunAction(ctx, "run/"+e.runID+"/step/"+step.ID+"/approval-request/attempt/"+strconv.Itoa(attempt), func(actionCtx context.Context) ([]byte, error) {
			if err := requester.Request(actionCtx, e.runID, step.ID); err != nil {
				return nil, err
			}
			return []byte(`{}`), nil
		}); err != nil {
			return &stepUpdate{Status: StepFailed, Attempt: attempt + 1, ErrorCode: string(ErrorCodeStepFailed), Detail: err.Error(), EndedAt: nowPtr()}
		}
	}
	e.suspendRun(ctx, step.ID)
	payload, timedOut, ok := e.invocation.Wait(ctx, "approval."+step.ID, step.Timeout)
	e.resumeRun(ctx, step.ID)
	if !ok {
		return &stepUpdate{Status: StepFailed, Attempt: attempt + 1, EndedAt: nowPtr(), Detail: errWaitCancelled.Error()}
	}
	if timedOut {
		return &stepUpdate{
			Status:    StepFailed,
			Attempt:   attempt + 1,
			EndedAt:   nowPtr(),
			ErrorCode: string(ErrorCodeStepTimeout),
		}
	}
	var decision struct {
		Decision string `json:"decision"`
	}
	if err := json.Unmarshal(payload, &decision); err != nil || decision.Decision == "" {
		return &stepUpdate{Status: StepFailed, Attempt: attempt + 1, ErrorCode: string(ErrorCodeStepFailed), Detail: "approval signal payload must carry a decision", EndedAt: nowPtr()}
	}
	if decision.Decision != "approve" {
		return &stepUpdate{Status: StepFailed, Attempt: attempt + 1, ErrorCode: string(ErrorCodeApprovalRejected), Detail: "approval was rejected", EndedAt: nowPtr()}
	}
	return &stepUpdate{Status: StepSucceeded, Attempt: attempt + 1, EndedAt: nowPtr(), Output: payload}
}
