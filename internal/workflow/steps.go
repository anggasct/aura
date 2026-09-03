package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/durable"
)

const maxInlineOutputBytes = 64 << 10

func (e *stepExecution) run(ctx context.Context) error {
	e.outputs = map[string]json.RawMessage{}
	e.statuses = map[string]string{}
	e.failedStep = ""
	done := make(map[string]chan struct{}, len(e.graph.Order))
	for _, stepID := range e.graph.Order {
		done[stepID] = make(chan struct{})
	}
	var wg sync.WaitGroup
	for _, stepID := range e.graph.Order {
		step := e.graph.ByStep[stepID]
		wg.Add(1)
		go func(step *StepSpec) {
			defer wg.Done()
			e.runStep(ctx, step, done)
		}(step)
	}
	wg.Wait()
	persistCtx := context.WithoutCancel(ctx)
	if err := ctx.Err(); err != nil {
		if setErr := e.interpreter.store.SetRunStatus(persistCtx, e.runID, RunCancelled); setErr != nil {
			return setErr
		}
		return err
	}
	terminal := RunSucceeded
	if e.failedStep != "" {
		terminal = RunFailed
	}
	if err := e.interpreter.store.SetRunStatus(persistCtx, e.runID, terminal); err != nil {
		return err
	}
	return nil
}

// runStep waits for dependencies, evaluates its condition, and executes
// within the concurrency bound; terminal transitions persist per step.
func (e *stepExecution) runStep(ctx context.Context, step *StepSpec, done map[string]chan struct{}) {
	defer close(done[step.ID])
	for _, dependency := range step.DependsOn {
		select {
		case <-done[dependency]:
		case <-ctx.Done():
			return
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return
	}
	if e.isFailed() {
		return
	}
	for _, dependency := range step.DependsOn {
		if e.statusFor(dependency) == StepSkipped {
			e.terminal(ctx, step.ID, &stepUpdate{Status: StepSkipped})
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
			e.terminal(ctx, step.ID, &stepUpdate{Status: StepSkipped})
			return
		}
	}

	attempt := 0
	for {
		update, ended := e.executeAttempt(ctx, step, attempt)
		if ended {
			return
		}
		if update.ErrorCode == "" {
			e.terminal(ctx, step.ID, update)
			e.recordSuccess(step.ID, update.Output)
			return
		}
		if attempt >= step.Retry.Attempts {
			e.terminal(ctx, step.ID, update)
			e.failRun(ctx, step.ID, &Error{Code: ErrorCode(update.ErrorCode), Detail: fmt.Sprintf("step %s exhausted %d attempts", step.ID, attempt+1)})
			return
		}
		e.terminal(ctx, step.ID, &stepUpdate{Status: StepFailed, Attempt: attempt, ErrorCode: update.ErrorCode})
		if err := e.invocation.Sleep(step.Retry.Backoff); err != nil {
			return
		}
		attempt++
	}
}

// executeAttempt runs one attempt under the timeout timer; ended reports a
// cancelled invocation.
func (e *stepExecution) executeAttempt(ctx context.Context, step *StepSpec, attempt int) (*stepUpdate, bool) {
	e.terminal(ctx, step.ID, &stepUpdate{Status: StepRunning, Attempt: attempt})
	e.interpreter.acquire()
	defer e.interpreter.release()

	result := make(chan *stepUpdate, 1)
	go func() {
		result <- e.invokeExecutor(ctx, step, attempt)
	}()
	timer := e.invocation.Timer(step.Timeout)
	select {
	case update := <-result:
		update.Attempt = attempt + 1
		return update, false
	case <-timer:
		return &stepUpdate{
			Status:    StepFailed,
			Attempt:   attempt + 1,
			EndedAt:   nowPtr(),
			ErrorCode: string(ErrorCodeStepTimeout),
		}, false
	case <-ctx.Done():
		return nil, true
	}
}

func (e *stepExecution) invokeExecutor(ctx context.Context, step *StepSpec, attempt int) *stepUpdate {
	switch step.Executor.Kind {
	case KindAgent:
		return e.runAgentStep(ctx, step)
	case KindTool:
		return e.runToolStep(ctx, step)
	case KindWait:
		return e.runWaitStep(ctx, step, attempt)
	case KindApproval:
		return e.runApprovalStep(ctx, step, attempt)
	default:
		return &stepUpdate{Status: StepFailed, ErrorCode: string(ErrorCodeExecutorInvalid), Detail: fmt.Sprintf("kind %q is not executable", step.Executor.Kind), EndedAt: nowPtr()}
	}
}

func (e *stepExecution) runAgentStep(ctx context.Context, step *StepSpec) *stepUpdate {
	runner := e.interpreter.options.Agents
	resolver := e.interpreter.options.AgentResolver
	if runner == nil || resolver == nil {
		return &stepUpdate{Status: StepFailed, ErrorCode: string(ErrorCodeExecutorInvalid), Detail: "no agent runner is wired", EndedAt: nowPtr()}
	}
	var resolved auraagent.Definition
	var err error
	if step.Executor.AgentID != nil && *step.Executor.AgentID != "" {
		prefer := *step.Executor.AgentID
		resolved, err = resolver.Resolve(nil, &prefer)
	} else {
		resolved, err = resolver.Resolve(step.Executor.RequiredCapabilities, nil)
	}
	if err != nil {
		return &stepUpdate{Status: StepFailed, ErrorCode: string(ErrorCodeExecutorInvalid), Detail: err.Error(), EndedAt: nowPtr()}
	}
	output, err := runner.Run(ctx, &resolved, &ExecutionContext{
		Objective:   e.input.Objective,
		Resources:   e.input.Resources,
		Artifacts:   e.input.Artifacts,
		Permissions: e.input.Permissions,
		Metadata:    e.input.Metadata,
	})
	if err != nil {
		return &stepUpdate{Status: StepFailed, ErrorCode: string(ErrorCodeSpecInvalid), Detail: err.Error(), EndedAt: nowPtr()}
	}
	return &stepUpdate{Status: StepSucceeded, Output: []byte(output), EndedAt: nowPtr()}
}

func (e *stepExecution) runToolStep(ctx context.Context, step *StepSpec) *stepUpdate {
	runner := e.interpreter.options.Tools
	if runner == nil || step.Executor.ToolID == nil {
		return &stepUpdate{Status: StepFailed, ErrorCode: string(ErrorCodeExecutorInvalid), Detail: "no tool runner is wired", EndedAt: nowPtr()}
	}
	output, err := runner.Invoke(ctx, *step.Executor.ToolID, json.RawMessage(`{}`))
	if err != nil {
		return &stepUpdate{Status: StepFailed, ErrorCode: string(ErrorCodeSpecInvalid), Detail: err.Error(), EndedAt: nowPtr()}
	}
	return &stepUpdate{Status: StepSucceeded, Output: []byte(output), EndedAt: nowPtr()}
}

// runWaitStep suspends on wait.<step_id>; the signal payload becomes the
// step output.
func (e *stepExecution) runWaitStep(ctx context.Context, step *StepSpec, attempt int) *stepUpdate {
	_ = e.interpreter.store.SetRunStatus(ctx, e.runID, RunSuspended)
	payload, ok := e.invocation.Signal("wait." + step.ID)
	_ = e.interpreter.store.SetRunStatus(ctx, e.runID, RunRunning)
	if !ok {
		return &stepUpdate{Status: StepFailed, Attempt: attempt + 1, ErrorCode: "", EndedAt: nowPtr(), Detail: errWaitCancelled.Error()}
	}
	if len(payload) > maxInlineOutputBytes {
		return &stepUpdate{Status: StepSucceeded, Attempt: attempt + 1, EndedAt: nowPtr(), Output: bounded(payload)}
	}
	return &stepUpdate{Status: StepSucceeded, Attempt: attempt + 1, EndedAt: nowPtr(), Output: payload}
}

// runApprovalStep requests approval bound to run+step and suspends on
// approval.<step_id>; a reject payload routes the run to failure.
func (e *stepExecution) runApprovalStep(ctx context.Context, step *StepSpec, attempt int) *stepUpdate {
	if requester := e.interpreter.options.Approvals; requester != nil {
		if err := requester.Request(ctx, e.runID, step.ID); err != nil {
			return &stepUpdate{Status: StepFailed, Attempt: attempt + 1, ErrorCode: string(ErrorCodeSpecInvalid), Detail: err.Error(), EndedAt: nowPtr()}
		}
	}
	_ = e.interpreter.store.SetRunStatus(ctx, e.runID, RunSuspended)
	payload, ok := e.invocation.Signal("approval." + step.ID)
	_ = e.interpreter.store.SetRunStatus(ctx, e.runID, RunRunning)
	if !ok {
		return &stepUpdate{Status: StepFailed, Attempt: attempt + 1, EndedAt: nowPtr(), Detail: errWaitCancelled.Error()}
	}
	var decision struct {
		Decision string `json:"decision"`
	}
	if err := json.Unmarshal(payload, &decision); err != nil || decision.Decision == "" {
		return &stepUpdate{Status: StepFailed, Attempt: attempt + 1, ErrorCode: string(ErrorCodeSpecInvalid), Detail: "approval signal payload must carry a decision", EndedAt: nowPtr()}
	}
	if decision.Decision != "approve" {
		return &stepUpdate{Status: StepFailed, Attempt: attempt + 1, ErrorCode: string(ErrorCodeSpecInvalid), Detail: "approval was rejected", EndedAt: nowPtr()}
	}
	return &stepUpdate{Status: StepSucceeded, Attempt: attempt + 1, EndedAt: nowPtr(), Output: payload}
}

type errWaitError struct{}

func (errWaitError) Error() string { return "wait ended by cancellation" }

var errWaitCancelled = errWaitError{}

func nowPtr() *time.Time {
	now := time.Now().UTC()
	return &now
}

// bounded trims inline output to the bounded inline size.
func bounded(payload []byte) []byte {
	if len(payload) > maxInlineOutputBytes {
		return payload[:maxInlineOutputBytes]
	}
	return payload
}

// terminal persists one step transition and records handler state. The
// write uses a context detached from the invocation so terminal rows stay
// durable even when the run is cancelled mid-step.
func (e *stepExecution) terminal(ctx context.Context, stepID string, update *stepUpdate) {
	if update.Status != StepRunning {
		e.mu.Lock()
		e.statuses[stepID] = update.Status
		e.mu.Unlock()
	}
	tx, err := e.interpreter.store.BeginStepTransaction(context.WithoutCancel(ctx))
	if err != nil {
		return
	}
	if err := UpdateStep(context.WithoutCancel(ctx), tx, e.runID, stepID, update); err != nil {
		_ = tx.Rollback()
		return
	}
	if update.Status != StepRunning && (update.Status == StepSucceeded || update.Status == StepFailed || update.Status == StepSkipped) {
		if err := e.interpreter.store.SetRunStatusTx(context.WithoutCancel(ctx), tx, e.runID, RunRunning); err != nil {
			_ = tx.Rollback()
			return
		}
	}
	_ = tx.Commit()
}

func (e *stepExecution) failRun(ctx context.Context, stepID string, err error) {
	e.mu.Lock()
	e.failedStep = stepID
	e.statuses[stepID] = StepFailed
	e.mu.Unlock()
	if e.interpreter.options.Logger != nil {
		e.interpreter.options.Logger.WarnContext(ctx, "workflow step failed", "step_id", stepID, "error", err.Error())
	}
}

// statusFor reads one step status under the working-state lock.
func (e *stepExecution) statusFor(stepID string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.statuses[stepID]
}

// isFailed reports whether any step failed the run.
func (e *stepExecution) isFailed() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.failedStep != ""
}

// recordSuccess records a succeeded step's status and output.
func (e *stepExecution) recordSuccess(stepID string, output json.RawMessage) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.statuses[stepID] = StepSucceeded
	e.outputs[stepID] = output
}

// resolvedStatus reads a status for condition evaluation.
func (e *stepExecution) resolvedStatus(stepID string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	status, ok := e.statuses[stepID]
	return status, ok
}

// resolvedOutput reads an output for condition evaluation.
func (e *stepExecution) resolvedOutput(stepID string) (json.RawMessage, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	output, ok := e.outputs[stepID]
	return output, ok
}

// evaluate executes the conjunction; missing references compare as null.
func (e *stepExecution) evaluate(c *condition) bool {
	for index := range c.comparisons {
		if !e.evaluateComparison(&c.comparisons[index]) {
			return false
		}
	}
	return true
}

func (e *stepExecution) evaluateComparison(cmp *comparison) bool {
	left, leftText := e.resolveOperand(cmp.left)
	right, rightText := e.resolveOperand(cmp.right)
	if leftText == "null" || rightText == "null" {
		equal := leftText == "null" && rightText == "null"
		return equal == (cmp.op == "==")
	}
	equal := jsonScalarEqual(left, leftText, right, rightText)
	if cmp.op == "!=" {
		return !equal
	}
	return equal
}

func jsonScalarEqual(left any, leftText string, right any, rightText string) bool {
	if leftText == rightText {
		return true
	}
	return fmt.Sprint(left) == fmt.Sprint(right)
}

// resolveOperand materializes one operand: references read step state and
// outputs; literals carry their own values.
func (e *stepExecution) resolveOperand(op operand) (value any, text string) {
	if op.ref == nil {
		switch {
		case op.num != nil:
			return *op.num, strconv.FormatInt(*op.num, 10)
		case op.flag != nil:
			return *op.flag, strconv.FormatBool(*op.flag)
		case op.nilText:
			return nil, "null"
		default:
			return op.text, op.text
		}
	}
	if op.ref.field == "status" {
		status, ok := e.resolvedStatus(op.ref.stepID)
		if !ok {
			return nil, "null"
		}
		return status, status
	}
	output, hasOutput := e.resolvedOutput(op.ref.stepID)
	if !hasOutput {
		return nil, "null"
	}
	resolved, text, found := extractJSONPath(output, op.ref.jsonKey, op.ref.index)
	if !found {
		return nil, "null"
	}
	return resolved, text
}

// extractJSONPath walks output JSON by keys and array indices.
func extractJSONPath(output []byte, keys []string, indices []int) (value any, text string, ok bool) {
	if len(output) == 0 {
		return nil, "", false
	}
	var document any
	if err := json.Unmarshal(output, &document); err != nil {
		return nil, "", false
	}
	current := document
	keyIndex := 0
	for _, key := range keys {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, "", false
		}
		value, ok := object[key]
		if !ok {
			return nil, "", false
		}
		current = value
		if keyIndex < len(indices) {
			array, ok := current.([]any)
			if !ok {
				return nil, "", false
			}
			index := indices[keyIndex]
			if index < 0 || index >= len(array) {
				return nil, "", false
			}
			current = array[index]
		}
		keyIndex++
	}
	return current, fmt.Sprint(current), true
}

// acquire bounds concurrently running steps; overflow waits, never drops.
func (i *Interpreter) acquire() {
	i.inflight <- struct{}{}
}

func (i *Interpreter) release() {
	<-i.inflight
}

var (
	_ = durable.RunRunning
	_ = context.Context(nil)
)
