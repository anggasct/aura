package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	auraagent "github.com/anggasct/aura/internal/agent"
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
		update, ended := e.executeAttempt(ctx, step, attempt)
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

// executeAttempt runs one attempt under the timeout timer; ended reports a
// cancelled invocation.
func (e *stepExecution) executeAttempt(ctx context.Context, step *StepSpec, attempt int) (*stepUpdate, bool) {
	if err := e.terminal(ctx, step.ID, &stepUpdate{Status: StepRunning, Attempt: attempt}); err != nil {
		e.failRun(ctx, step.ID, err)
		return nil, true
	}
	e.interpreter.acquire()
	defer e.interpreter.release()

	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	result := make(chan *stepUpdate, 1)
	go func() {
		result <- e.invokeExecutor(attemptCtx, step, attempt)
	}()
	timer := e.invocation.Timer(step.Timeout)
	select {
	case update := <-result:
		update.Attempt = attempt + 1
		return update, false
	case <-timer:
		cancel()
		// Every executor kind observes attempt cancellation, so the
		// attempt goroutine always exits; draining keeps an abandoned
		// wait/approval attempt from holding a signal waiter slot across
		// the retry.
		<-result
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
		return &stepUpdate{Status: StepFailed, ErrorCode: string(ErrorCodeStepFailed), Detail: err.Error(), EndedAt: nowPtr()}
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
		return &stepUpdate{Status: StepFailed, ErrorCode: string(ErrorCodeStepFailed), Detail: err.Error(), EndedAt: nowPtr()}
	}
	return &stepUpdate{Status: StepSucceeded, Output: []byte(output), EndedAt: nowPtr()}
}

// runWaitStep suspends on wait.<step_id>; the signal payload becomes the
// step output.
func (e *stepExecution) runWaitStep(ctx context.Context, step *StepSpec, attempt int) *stepUpdate {
	e.suspendRun(ctx, step.ID)
	payload, ok := e.invocation.Signal(ctx, "wait."+step.ID)
	e.resumeRun(ctx, step.ID)
	if !ok {
		return &stepUpdate{Status: StepFailed, Attempt: attempt + 1, ErrorCode: "", EndedAt: nowPtr(), Detail: errWaitCancelled.Error()}
	}
	return &stepUpdate{Status: StepSucceeded, Attempt: attempt + 1, EndedAt: nowPtr(), Output: payload}
}

// runApprovalStep requests approval bound to run+step and suspends on
// approval.<step_id>; a reject payload routes the run to failure.
func (e *stepExecution) runApprovalStep(ctx context.Context, step *StepSpec, attempt int) *stepUpdate {
	if requester := e.interpreter.options.Approvals; requester != nil {
		if err := requester.Request(ctx, e.runID, step.ID); err != nil {
			return &stepUpdate{Status: StepFailed, Attempt: attempt + 1, ErrorCode: string(ErrorCodeStepFailed), Detail: err.Error(), EndedAt: nowPtr()}
		}
	}
	e.suspendRun(ctx, step.ID)
	payload, ok := e.invocation.Signal(ctx, "approval."+step.ID)
	e.resumeRun(ctx, step.ID)
	if !ok {
		return &stepUpdate{Status: StepFailed, Attempt: attempt + 1, EndedAt: nowPtr(), Detail: errWaitCancelled.Error()}
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

type errWaitError struct{}

func (errWaitError) Error() string { return "wait ended by cancellation" }

var errWaitCancelled = errWaitError{}

func nowPtr() *time.Time {
	now := time.Now().UTC()
	return &now
}

// sinkOutput routes outputs over the inline limit through the artifact
// sink, leaving a bounded digest reference inline; without a sink the
// failure surfaces instead of truncating into an invalid row.
func (e *stepExecution) sinkOutput(ctx context.Context, update *stepUpdate) error {
	if len(update.Output) <= maxInlineOutputBytes {
		return nil
	}
	sink := e.interpreter.options.Artifacts
	if sink == nil {
		return codedError(ErrorCodeStepFailed, fmt.Sprintf("output of %d bytes exceeds the %d byte inline limit and no artifact sink is wired", len(update.Output), maxInlineOutputBytes))
	}
	digest, err := sink.Put(context.WithoutCancel(ctx), update.Output)
	if err != nil {
		return codedError(ErrorCodeStepFailed, "store output artifact: "+err.Error())
	}
	update.ArtifactDigest = digest
	update.Output = json.RawMessage(fmt.Sprintf(`{"artifact_digest":%q}`, digest))
	return nil
}

// suspendRun marks the step as awaiting a signal and moves the run row to
// suspended when it is the only waiter. The waiter bookkeeping happens
// under statusMu, but the database write never does: statusMu only guards
// the in-memory awaiting set, and every run-status write takes writeMu, so
// no code path holds a database write while waiting on the mutex that
// another database writer needs.
func (e *stepExecution) suspendRun(ctx context.Context, stepID string) {
	e.statusMu.Lock()
	e.awaiting[stepID] = true
	onlyWaiter := len(e.awaiting) == 1
	e.statusMu.Unlock()
	if onlyWaiter {
		e.writeRunStatus(ctx, RunSuspended)
	}
}

// resumeRun clears the step's waiter flag; the run row returns to running
// only when no sibling remains suspended.
func (e *stepExecution) resumeRun(ctx context.Context, stepID string) {
	e.statusMu.Lock()
	delete(e.awaiting, stepID)
	noneLeft := len(e.awaiting) == 0 && ctx.Err() == nil
	e.statusMu.Unlock()
	if noneLeft {
		e.writeRunStatus(ctx, RunRunning)
	}
}

func (e *stepExecution) writeRunStatus(ctx context.Context, status string) {
	e.interpreter.writeMu.Lock()
	defer e.interpreter.writeMu.Unlock()
	writeCtx := context.WithoutCancel(ctx)
	if err := e.interpreter.store.SetRunStatus(writeCtx, e.runID, status); err != nil && e.interpreter.options.Logger != nil {
		e.interpreter.options.Logger.WarnContext(writeCtx, "workflow run status persist failed", "status", status, "error", err.Error())
	}
}

// terminal persists one step transition and records handler state. The
// write uses a context detached from the invocation so terminal rows stay
// durable even when the run is cancelled mid-step. A run-terminal step
// keeps a concurrently suspended run suspended instead of forcing it back
// to running.
func (e *stepExecution) terminal(ctx context.Context, stepID string, update *stepUpdate) error {
	if update.Status != StepRunning {
		e.mu.Lock()
		e.statuses[stepID] = update.Status
		e.mu.Unlock()
	}
	// The whole transaction runs under writeMu, acquired before the
	// transaction opens: a writeMu holder therefore never owns the SQLite
	// write lock while waiting on the mutex, and a mutex holder never
	// waits on a SQLite lock the next writeMu waiter owns. Without this
	// ordering a step transaction holding the SQLite write lock would
	// deadlock against a suspend/resume status write busy-waiting for it.
	e.interpreter.writeMu.Lock()
	defer e.interpreter.writeMu.Unlock()
	tx, err := e.interpreter.store.BeginStepTransaction(context.WithoutCancel(ctx))
	if err != nil {
		return fmt.Errorf("begin step %s transition: %w", stepID, err)
	}
	if err := UpdateStep(context.WithoutCancel(ctx), tx, e.runID, stepID, update); err != nil {
		_ = tx.Rollback()
		return err
	}
	if update.Status == StepSucceeded || update.Status == StepFailed || update.Status == StepSkipped {
		e.statusMu.Lock()
		runStatus := RunRunning
		if len(e.awaiting) > 0 {
			runStatus = RunSuspended
		}
		e.statusMu.Unlock()
		if err := e.interpreter.store.SetRunStatusTx(context.WithoutCancel(ctx), tx, e.runID, runStatus); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit step %s transition: %w", stepID, err)
	}
	return nil
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
