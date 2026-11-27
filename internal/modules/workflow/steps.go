package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const maxInlineOutputBytes = 64 << 10

func (e *stepExecution) run(ctx context.Context) error {
	e.outputs = map[string]json.RawMessage{}
	e.statuses = map[string]string{}
	e.labels = newLabelLedger()
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
	if !e.gate(ctx, step) {
		return
	}

	attempt := 0
	for {
		if !e.budget.admit() {
			e.exhaustBudget(ctx, step, attempt, "max_steps")
			return
		}
		update, ended := e.executeAttempt(ctx, step, attempt)
		if ended {
			return
		}
		if !e.budget.spend(update.Tokens) {
			e.exhaustBudget(ctx, step, update.Attempt, "max_tokens")
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
			e.recordSuccess(step.ID, update)
			return
		}
		if attempt >= step.Retry.Attempts || finalFailure(update.ErrorCode) {
			if err := e.terminal(ctx, step.ID, update); err != nil {
				e.failRun(ctx, step.ID, err)
				return
			}
			e.failRun(ctx, step.ID, finalStepError(step.ID, attempt, update.ErrorCode))
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

func (e *stepExecution) gate(ctx context.Context, step *StepSpec) bool {
	var parsed *condition
	var parseErr error
	if step.Condition != nil {
		parsed, parseErr = parseCondition(*step.Condition)
	}
	e.labels.steer(step, parsed)
	for _, dependency := range step.DependsOn {
		if e.statusFor(dependency) == StepSkipped {
			e.skip(ctx, step)
			return false
		}
	}
	if step.Condition == nil {
		return true
	}
	if parseErr != nil {
		e.failRun(ctx, step.ID, parseErr)
		return false
	}
	if !e.evaluate(parsed) {
		e.skip(ctx, step)
		return false
	}
	return true
}

func (e *stepExecution) skip(ctx context.Context, step *StepSpec) {
	if err := e.terminal(ctx, step.ID, &stepUpdate{Status: StepSkipped}); err != nil {
		e.failRun(ctx, step.ID, err)
	}
}

func (e *stepExecution) executeAttempt(ctx context.Context, step *StepSpec, attempt int) (*stepUpdate, bool) {
	if err := e.terminal(ctx, step.ID, &stepUpdate{Status: StepRunning, Attempt: attempt}); err != nil {
		e.failRun(ctx, step.ID, err)
		return nil, true
	}
	if step.Executor.Kind != KindWorkflow {
		e.interpreter.acquire()
		defer e.interpreter.release()
	}

	attemptCtx, cancel := context.WithTimeout(ctx, step.Timeout)
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
		return e.runAgentStep(ctx, step, attempt)
	case KindTool:
		return e.runToolStep(ctx, step, attempt)
	case KindWait:
		return e.runWaitStep(ctx, step, attempt)
	case KindApproval:
		return e.runApprovalStep(ctx, step, attempt)
	case KindWorkflow:
		return e.runWorkflowStep(ctx, step)
	default:
		return &stepUpdate{Status: StepFailed, ErrorCode: string(ErrorCodeExecutorInvalid), Detail: fmt.Sprintf("kind %q is not executable", step.Executor.Kind), EndedAt: nowPtr()}
	}
}

func (e *stepExecution) runAgentStep(ctx context.Context, step *StepSpec, attempt int) *stepUpdate {
	runner := e.interpreter.options.AgentSteps
	if runner == nil {
		return &stepUpdate{Status: StepFailed, ErrorCode: string(ErrorCodeExecutorInvalid), Detail: "no agent runner is wired", EndedAt: nowPtr()}
	}
	origin := e.originFor(step.ID)
	call := &AgentStepCall{
		RunID: e.runID, StepID: step.ID, Attempt: attempt, Origin: origin,
		DefinitionID: e.spec.ID, DefinitionVersion: e.spec.Version, DefinitionDigest: e.definitionDigest,
		Executor: step.Executor, Input: *e.input, Invocation: e.invocation,
	}
	result, err := runner.RunStep(ctx, call)
	if err != nil {
		return &stepUpdate{Status: StepFailed, ErrorCode: string(failureCode(err)), Detail: err.Error(), EndedAt: nowPtr(), Tokens: result.Tokens}
	}
	tainted, private := e.labels.outputOf(step, e.origin.Tainted, reportedLabels{tainted: result.Tainted, private: result.Private})
	return &stepUpdate{Status: StepSucceeded, Output: []byte(result.Output), EndedAt: nowPtr(), Tokens: result.Tokens, Tainted: tainted, Private: private}
}

func (e *stepExecution) originFor(stepID string) RunOrigin {
	origin := e.origin
	origin.Tainted = e.origin.Tainted || e.labels.isSteered(stepID)
	return origin
}

func failureCode(err error) ErrorCode {
	if coded, ok := fault.CodeOf[ErrorCode](err); ok {
		return coded
	}
	return ErrorCodeStepFailed
}

func toolArguments(step *StepSpec) json.RawMessage {
	if len(step.Executor.ToolArgs) == 0 {
		return json.RawMessage(`{}`)
	}
	return append(json.RawMessage(nil), step.Executor.ToolArgs...)
}

func (e *stepExecution) runToolStep(ctx context.Context, step *StepSpec, attempt int) *stepUpdate {
	runner := e.interpreter.options.Tools
	if runner == nil || step.Executor.ToolID == nil {
		return &stepUpdate{Status: StepFailed, ErrorCode: string(ErrorCodeExecutorInvalid), Detail: "no tool runner is wired", EndedAt: nowPtr()}
	}
	if !e.origin.Complete() {
		return &stepUpdate{Status: StepFailed, ErrorCode: string(ErrorCodeOriginIncomplete), Detail: "run " + e.runID + " has no complete origin", EndedAt: nowPtr()}
	}
	approval, err := e.interpreter.requireApproval(ctx, e.spec.ID, e.spec.Version, e.definitionDigest)
	if err != nil {
		return &stepUpdate{Status: StepFailed, ErrorCode: string(failureCode(err)), Detail: err.Error(), EndedAt: nowPtr()}
	}
	origin := e.originFor(step.ID)
	origin.Tainted = origin.Tainted || approval.Tainted
	call := &StepCall{
		RunID: e.runID, StepID: step.ID, Attempt: attempt, Origin: origin, DefinitionDigest: e.definitionDigest,
		DefinitionID: e.spec.ID, DefinitionVersion: e.spec.Version, Approval: *approval,
	}
	result, err := runner.Invoke(WithStepCall(ctx, call), *step.Executor.ToolID, toolArguments(step))
	if err != nil {
		return &stepUpdate{Status: StepFailed, ErrorCode: string(failureCode(err)), Detail: err.Error(), EndedAt: nowPtr()}
	}
	tainted, private := e.labels.outputOf(step, origin.Tainted, reportedLabels{tainted: result.Tainted})
	return &stepUpdate{Status: StepSucceeded, Output: []byte(result.Output), EndedAt: nowPtr(), Tainted: tainted, Private: private}
}

func (e *stepExecution) runWaitStep(ctx context.Context, step *StepSpec, attempt int) *stepUpdate {
	if update, failed := e.bindWaitCorrelation(ctx, step, attempt); failed {
		return update
	}
	e.suspendRun(ctx, step.ID)
	payload, ok := e.awaitSignal(ctx, "wait."+step.ID)
	e.resumeRun(ctx, step.ID)
	if !ok {
		return &stepUpdate{Status: StepFailed, Attempt: attempt + 1, ErrorCode: "", EndedAt: nowPtr(), Detail: errWaitCancelled.Error()}
	}
	return &stepUpdate{Status: StepSucceeded, Attempt: attempt + 1, EndedAt: nowPtr(), Output: payload}
}

func (e *stepExecution) runApprovalStep(ctx context.Context, step *StepSpec, attempt int) *stepUpdate {
	if requester := e.interpreter.options.Approvals; requester != nil {
		if err := requester.Request(ctx, e.runID, step.ID); err != nil {
			return &stepUpdate{Status: StepFailed, Attempt: attempt + 1, ErrorCode: string(ErrorCodeStepFailed), Detail: err.Error(), EndedAt: nowPtr()}
		}
	}
	e.suspendRun(ctx, step.ID)
	payload, ok := e.awaitSignal(ctx, "approval."+step.ID)
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

func (e *stepExecution) suspendRun(ctx context.Context, stepID string) {
	e.statusMu.Lock()
	e.awaiting[stepID] = true
	onlyWaiter := len(e.awaiting) == 1
	e.statusMu.Unlock()
	if onlyWaiter {
		e.writeRunStatus(ctx, RunSuspended)
	}
}

func (e *stepExecution) awaitSignal(ctx context.Context, name string) (json.RawMessage, bool) {
	e.interpreter.release()
	payload, ok := e.invocation.Signal(ctx, name)
	e.interpreter.acquire()
	return payload, ok
}

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

func (e *stepExecution) terminal(ctx context.Context, stepID string, update *stepUpdate) error {
	if update.Status != StepRunning {
		e.mu.Lock()
		e.statuses[stepID] = update.Status
		e.mu.Unlock()
	}
	e.interpreter.writeMu.Lock()
	defer e.interpreter.writeMu.Unlock()
	persistCtx := context.WithoutCancel(ctx)
	return sqlite.RunInTx(persistCtx, e.interpreter.store.db, func(txCtx context.Context) error {
		if err := e.interpreter.store.updateStep(txCtx, e.runID, stepID, update); err != nil {
			return err
		}
		if update.Status == StepSucceeded || update.Status == StepFailed || update.Status == StepSkipped {
			e.statusMu.Lock()
			runStatus := RunRunning
			if len(e.awaiting) > 0 {
				runStatus = RunSuspended
			}
			e.statusMu.Unlock()
			if err := e.interpreter.store.SetRunStatus(txCtx, e.runID, runStatus); err != nil {
				return err
			}
		}
		return nil
	})
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

func (e *stepExecution) statusFor(stepID string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.statuses[stepID]
}

func (e *stepExecution) isFailed() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.failedStep != ""
}

func (e *stepExecution) recordSuccess(stepID string, update *stepUpdate) {
	e.labels.record(stepID, update.Tainted, update.Private)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.statuses[stepID] = StepSucceeded
	e.outputs[stepID] = update.Output
}

func (e *stepExecution) resolvedStatus(stepID string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	status, ok := e.statuses[stepID]
	return status, ok
}

func (e *stepExecution) resolvedOutput(stepID string) (json.RawMessage, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	output, ok := e.outputs[stepID]
	return output, ok
}

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
	if leftNumber, ok := scalarNumber(left); ok {
		if rightNumber, ok := scalarNumber(right); ok {
			return leftNumber == rightNumber
		}
	}
	return fmt.Sprint(left) == fmt.Sprint(right)
}

func scalarNumber(value any) (float64, bool) {
	switch number := value.(type) {
	case float64:
		return number, true
	case int64:
		return float64(number), true
	default:
		return 0, false
	}
}

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

func (i *Interpreter) acquire() {
	i.inflight <- struct{}{}
}

func (i *Interpreter) release() {
	<-i.inflight
}

func finalFailure(code string) bool {
	switch ErrorCode(code) {
	case ErrorCodeApprovalRejected, ErrorCodeApprovalMissing, ErrorCodeApprovalSpent, ErrorCodePlanUnshowable,
		ErrorCodeCapabilityExceeded, ErrorCodePersonaMissing, ErrorCodeSessionInvalid, ErrorCodeExecutorInvalid,
		ErrorCodeNotPermitted, ErrorCodeBudgetExhausted:
		return true
	default:
		return false
	}
}

func finalStepError(stepID string, attempt int, code string) error {
	if finalFailure(code) {
		return codedError(ErrorCode(code), fmt.Sprintf("step %s failed with %s, which is not retried", stepID, code))
	}
	return codedError(ErrorCode(code), fmt.Sprintf("step %s exhausted %d attempts", stepID, attempt+1))
}
