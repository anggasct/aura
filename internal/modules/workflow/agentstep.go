package workflow

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/platform/turn/ingress"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
	"github.com/anggasct/aura/internal/platform/usage"
)

const (
	maxAgentEnvelopeBytes = 16 << 10
	maxAgentInputBytes    = 256 << 10
)

type AgentTurnRunnerDeps struct {
	Turns      StepTurns
	Events     StepEvents
	Sessions   RunSessionReader
	Tools      StepToolCatalog
	Runs       *Store
	Validation *ValidationDeps
	Logger     *slog.Logger
}

type AgentTurnRunner struct {
	turns      StepTurns
	events     StepEvents
	sessions   RunSessionReader
	tools      StepToolCatalog
	runs       *Store
	validation *ValidationDeps
	logger     *slog.Logger
}

func NewAgentTurnRunner(deps *AgentTurnRunnerDeps) (*AgentTurnRunner, error) {
	if deps == nil || deps.Turns == nil || deps.Events == nil || deps.Sessions == nil || deps.Tools == nil || deps.Runs == nil || deps.Validation == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "workflow agent turn runner needs turns, events, sessions, a tool catalog, a workflow store and validation dependencies")
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &AgentTurnRunner{
		turns: deps.Turns, events: deps.Events, sessions: deps.Sessions, tools: deps.Tools,
		runs: deps.Runs, validation: deps.Validation, logger: logger,
	}, nil
}

type agentAttempt struct {
	call      *AgentStepCall
	persona   string
	sessionID string
	approval  *Approval
	scope     *durable.TurnScope
	tally     *turnTally
	tainted   bool
	private   bool
	maxTokens int64
	pin       *turn.PersonaFingerprint
}

func (r *AgentTurnRunner) RunStep(ctx context.Context, call *AgentStepCall) (AgentStepResult, error) {
	if call == nil || call.Invocation == nil {
		return AgentStepResult{}, codedError(ErrorCodeExecutorInvalid, "agent step call needs an invocation")
	}
	attempt, err := r.admit(ctx, call)
	if err != nil {
		return AgentStepResult{}, r.refused(ctx, call, err)
	}
	text, err := agentInputText(call)
	if err != nil {
		return AgentStepResult{}, r.refused(ctx, call, err)
	}
	if err := r.ensureSession(ctx, attempt); err != nil {
		return AgentStepResult{}, r.refused(ctx, call, err)
	}
	return r.drive(ctx, attempt, text)
}

func (r *AgentTurnRunner) admit(ctx context.Context, call *AgentStepCall) (*agentAttempt, error) {
	if !call.Origin.Complete() {
		return nil, codedError(ErrorCodeOriginIncomplete, "run "+call.RunID+" has no complete origin")
	}
	approval, err := r.runs.Approval(ctx, call.DefinitionID, call.DefinitionVersion)
	if err != nil {
		return nil, err
	}
	if _, err := checkStepApproval(&StepCall{
		DefinitionDigest: call.DefinitionDigest, DefinitionID: call.DefinitionID, DefinitionVersion: call.DefinitionVersion, Approval: *approval,
	}); err != nil {
		return nil, err
	}
	step := &StepSpec{ID: call.StepID, Executor: call.Executor}
	class := principal.ParseClass(call.Origin.PrincipalClass)
	if err := checkPersona(r.validation, call.Origin.AgentID, class); err != nil {
		return nil, err
	}
	if err := checkAgentStepCeiling(step, call.Origin.AgentID, class, r.validation); err != nil {
		return nil, err
	}
	persona := deref(call.Executor.AgentID)
	pin, err := r.pinnedPersona(approval, persona)
	if err != nil {
		return nil, err
	}
	for _, toolID := range call.Executor.Tools {
		if _, registered := r.tools.Definition(toolID, tools.Version); !registered {
			return nil, codedError(ErrorCodeToolUnavailable, "tool "+toolID+" is not registered on this host")
		}
	}
	maxTokens := int64(call.Executor.TokenBudget())
	if call.RunTokensLeft > 0 {
		maxTokens = min(maxTokens, call.RunTokensLeft)
	}
	return &agentAttempt{
		call:      call,
		persona:   persona,
		pin:       pin,
		sessionID: StepSessionID(call.RunID, call.StepID, call.Attempt),
		approval:  approval,
		tally:     &turnTally{},
		tainted:   call.Origin.Tainted || approval.Tainted,
		private:   hasWorkspaceTool(step) || slices.ContainsFunc(call.Inputs, func(input AgentStepInput) bool { return input.Private }),
		maxTokens: maxTokens,
	}, nil
}

func (r *AgentTurnRunner) pinnedPersona(approval *Approval, personaID string) (*turn.PersonaFingerprint, error) {
	pin, pinned, err := approval.personaPin(personaID)
	if err != nil || !pinned {
		return nil, err
	}
	definition, err := r.validation.Agents.Resolve(nil, &personaID)
	if err != nil {
		return nil, codedError(ErrorCodePersonaMissing, fmt.Sprintf("persona %q is not configured", personaID))
	}
	current := definition.Fingerprint()
	if current.SoulSHA256 != pin.SoulSHA256 || current.ModelRoute != pin.ModelRoute {
		return nil, codedError(ErrorCodeApprovalMissing, fmt.Sprintf("persona %q changed since definition %s version %d was approved", personaID, approval.DefinitionID, approval.DefinitionVersion))
	}
	return &turn.PersonaFingerprint{AgentID: personaID, SoulSHA256: pin.SoulSHA256, ModelRoute: pin.ModelRoute}, nil
}

func (r *AgentTurnRunner) ensureSession(ctx context.Context, attempt *agentAttempt) error {
	if err := r.runs.createStepSession(ctx, &stepSession{ID: attempt.sessionID, OwnerID: attempt.call.Origin.PrincipalID, PersonaID: attempt.persona}); err != nil {
		return err
	}
	stored, err := r.sessions.Get(ctx, attempt.sessionID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return codedError(ErrorCodeSessionInvalid, "step session "+attempt.sessionID+" does not exist")
	case err != nil:
		return codedError(ErrorCodeStepFailed, "read step session: "+err.Error())
	case stored.OwnerID != attempt.call.Origin.PrincipalID || stored.AgentID != attempt.persona:
		return codedError(ErrorCodeSessionInvalid, "step session "+attempt.sessionID+" does not belong to the run's origin and the step persona")
	}
	return nil
}

func (r *AgentTurnRunner) refused(ctx context.Context, call *AgentStepCall, err error) error {
	r.logger.WarnContext(ctx, "workflow agent step refused before the model", "component", "workflow",
		"run_id", call.RunID, "step_id", call.StepID, "attempt", call.Attempt, "code", string(failureCode(err)))
	return err
}

const (
	agentJournalAction  = "journal"
	agentJournalVersion = 4
)

type agentJournalRecord struct {
	Journal int `json:"journal"`
}

func decodeAgentJournal(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var record agentJournalRecord
	if err := decoder.Decode(&record); err != nil {
		return false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return false
	}
	return record.Journal == agentJournalVersion
}

func checkAgentJournalVersion(ctx context.Context, scope *durable.TurnScope) error {
	raw, err := scope.Invocation().RunAction(ctx, agentJournalAction, func(context.Context) ([]byte, error) {
		return json.Marshal(agentJournalRecord{Journal: agentJournalVersion})
	})
	if err != nil {
		return mapAgentTurnError(ctx, err)
	}
	if !decodeAgentJournal(raw) {
		return codedError(ErrorCodeRuntimeInternal, "agent step journal was written by another build")
	}
	return nil
}

func (r *AgentTurnRunner) drive(ctx context.Context, attempt *agentAttempt, text string) (AgentStepResult, error) {
	call := attempt.call
	scope := durable.NewTurnScopeWithin(call.Invocation, agentJournalPrefix(call))
	attempt.scope = scope
	if err := checkAgentJournalVersion(ctx, scope); err != nil {
		r.finished(ctx, attempt, string(failureCode(err)))
		return AgentStepResult{}, err
	}
	origin := call.Origin
	origin.Tainted = attempt.tainted
	turnCtx := durable.WithTurnScope(WithStepCall(ctx, &StepCall{
		RunID: call.RunID, StepID: call.StepID, Attempt: call.Attempt, Origin: origin, DefinitionDigest: call.DefinitionDigest,
		DefinitionID: call.DefinitionID, DefinitionVersion: call.DefinitionVersion, Approval: *attempt.approval,
	}), scope)

	tally := attempt.tally
	var execErr error
	for event, err := range r.turns.Execute(turnCtx, agentTurnRequest(ctx, attempt, text)) {
		if err != nil {
			execErr = err
			break
		}
		if execErr = ctx.Err(); execErr != nil {
			break
		}
		partial, err := tally.observe(&event)
		if err != nil {
			execErr = err
			break
		}
		if partial {
			continue
		}
		if execErr = r.persist(turnCtx, attempt, &event); execErr != nil {
			break
		}
	}
	if execErr != nil {
		err := mapAgentTurnError(ctx, execErr)
		r.finished(ctx, attempt, string(failureCode(err)))
		return AgentStepResult{Tokens: tally.tokens}, err
	}
	tainted := attempt.outputTainted()
	output, err := attempt.envelope()
	if err != nil {
		r.finished(ctx, attempt, string(ErrorCodeAgentOutputInvalid))
		return AgentStepResult{Tokens: tally.tokens}, err
	}
	r.finished(ctx, attempt, "succeeded")
	return AgentStepResult{Output: output, Tokens: tally.tokens, Tainted: tainted, Private: attempt.private}, nil
}

func agentJournalPrefix(call *AgentStepCall) string {
	return "run/" + call.RunID + "/step/" + call.StepID + "/attempt/" + strconv.Itoa(call.Attempt) + "/agent/"
}

func agentTurnRequest(ctx context.Context, attempt *agentAttempt, text string) *turn.TurnRequest {
	call := attempt.call
	deadline, _ := ctx.Deadline()
	return &turn.TurnRequest{
		TurnID:          attempt.sessionID,
		SessionID:       attempt.sessionID,
		IdempotencyKey:  attempt.sessionID,
		PrincipalID:     call.Origin.PrincipalID,
		PrincipalClass:  principal.ParseClass(call.Origin.PrincipalClass),
		Origin:          turn.OriginWorkflow,
		Parts:           []runtimeingress.InputPart{{Text: text}},
		Deadline:        deadline,
		Budget:          turn.Budget{MaxTokens: attempt.maxTokens},
		AgentID:         attempt.persona,
		Persona:         attempt.pin,
		InputTainted:    attempt.tainted,
		ToolRestriction: &turn.ToolRestriction{Allowed: append([]string{}, call.Executor.Tools...)},
	}
}

func (r *AgentTurnRunner) persist(ctx context.Context, attempt *agentAttempt, event *turn.Event) error {
	scope := attempt.scope
	event.ID = scope.NextEventID(attempt.sessionID)
	raw, err := scope.Invocation().RunAction(ctx, scope.NextClock(), func(context.Context) ([]byte, error) {
		return json.Marshal(time.Now().UTC())
	})
	if err != nil {
		return err
	}
	var clock time.Time
	if err := json.Unmarshal(raw, &clock); err != nil {
		return codedError(ErrorCodeStepFailed, "decode journaled clock: "+err.Error())
	}
	event.CreatedAt = clock.UTC()
	event.SessionID = attempt.sessionID
	event.TurnID = attempt.sessionID
	if event.Author == "" {
		event.Author = attempt.call.Origin.PrincipalID
	}
	if event.SchemaVersion == 0 {
		event.SchemaVersion = 1
	}
	if _, _, err := r.events.UpsertEvent(ctx, event); err != nil {
		return codedError(ErrorCodeStepFailed, "persist step event: "+err.Error())
	}
	return nil
}

func (r *AgentTurnRunner) finished(ctx context.Context, attempt *agentAttempt, outcome string) {
	tally := attempt.tally
	r.logger.InfoContext(ctx, "workflow agent step finished", "component", "workflow",
		"run_id", attempt.call.RunID, "step_id", attempt.call.StepID, "attempt", attempt.call.Attempt,
		"persona", attempt.persona, "tools", strings.Join(attempt.call.Executor.Tools, ","),
		"tool_calls", tally.toolCalls, "refused_calls", tally.refusedCalls, "tokens", tally.tokens,
		"tainted", attempt.outputTainted(), "private", attempt.private, "outcome", outcome)
}

func mapAgentTurnError(ctx context.Context, err error) error {
	switch {
	case fault.HasCode(err, turn.ErrorCodeToolBudgetExhausted):
		return codedError(ErrorCodeToolBudgetExhausted, err.Error())
	case fault.HasCode(err, turn.ErrorCodeBudgetExhausted), fault.HasCode(err, usage.ErrorCodeBudgetExceeded), errors.Is(err, usage.ErrBudgetExceeded):
		return codedError(ErrorCodeBudgetExhausted, "the agent step exceeded its token budget")
	case fault.HasCode(err, turn.ErrorCodePersonaMismatch):
		return codedError(ErrorCodeApprovalMissing, "the step persona changed after it was pinned")
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, context.DeadlineExceeded):
		return codedError(ErrorCodeStepTimeout, "the persona turn timeout elapsed")
	}
	if _, coded := fault.CodeOf[ErrorCode](err); coded {
		return err
	}
	return codedError(ErrorCodeStepFailed, err.Error())
}

func agentInputText(call *AgentStepCall) (string, error) {
	var text strings.Builder
	block := func(open, closing, body string) {
		text.WriteString(open + "\n" + body + "\n" + closing + "\n")
	}
	if call.Input.Objective != "" {
		block("[workflow objective]", "[/workflow objective]", turn.QuoteUntrusted(call.Input.Objective))
	}
	for _, resource := range call.Input.Resources {
		block("[workflow resource]", "[/workflow resource]", turn.QuoteUntrusted(resource.Kind+" "+resource.URI))
	}
	block("[workflow step prompt]", "[/workflow step prompt]", call.Executor.Prompt)
	for _, input := range call.Inputs {
		block("[workflow step output "+input.StepID+"]", "[/workflow step output]", turn.QuoteUntrusted(string(input.Output)))
	}
	if text.Len() > maxAgentInputBytes {
		return "", codedError(ErrorCodeExecutorInvalid, "agent step input exceeds "+strconv.Itoa(maxAgentInputBytes)+" bytes")
	}
	return text.String(), nil
}

type turnTally struct {
	tokens       int64
	toolCalls    int
	refusedCalls int
	webResult    bool
	finalText    string
	answered     bool
}

func (t *turnTally) observe(event *turn.Event) (partial bool, err error) {
	if event.Kind != turnstore.EventKindADK {
		return false, nil
	}
	decoded, err := turnstore.RuntimeEventToADK(event)
	if err != nil {
		return false, codedError(ErrorCodeStepFailed, "decode turn event: "+err.Error())
	}
	if decoded.Partial {
		return true, nil
	}
	if decoded.UsageMetadata != nil {
		t.tokens += int64(decoded.UsageMetadata.TotalTokenCount)
	}
	if decoded.Content == nil {
		return false, nil
	}
	called := false
	var answer strings.Builder
	for _, part := range decoded.Content.Parts {
		switch {
		case part.FunctionCall != nil:
			called = true
			t.toolCalls++
		case part.FunctionResponse != nil:
			t.observeToolResult(part.FunctionResponse)
		case part.Text != "" && !part.Thought:
			answer.WriteString(part.Text)
		}
	}
	if decoded.Content.Role == genai.RoleModel && !called {
		t.finalText, t.answered = answer.String(), true
	}
	return false, nil
}

func (t *turnTally) observeToolResult(response *genai.FunctionResponse) {
	failure, failed := response.Response["error"].(string)
	if !failed {
		t.webResult = t.webResult || isWebTool(response.Name)
		return
	}
	if strings.Contains(failure, string(toolbroker.ResultApprovalRequired)) || strings.Contains(failure, string(toolbroker.ResultUntrustedInputNoApprover)) {
		t.refusedCalls++
	}
}

type agentOutput struct {
	Text    string          `json:"text"`
	Tainted bool            `json:"tainted"`
	Private bool            `json:"private"`
	JSON    json.RawMessage `json:"json,omitempty"`
}

func (a *agentAttempt) outputTainted() bool {
	return a.tainted || a.tally.webResult
}

func (a *agentAttempt) envelope() (json.RawMessage, error) {
	tally, executor := a.tally, &a.call.Executor
	if !tally.answered {
		return nil, codedError(ErrorCodeAgentOutputInvalid, "the turn ended without a final answer")
	}
	output := agentOutput{Text: tally.finalText, Tainted: a.outputTainted(), Private: a.private}
	if executor.OutputMode() == OutputJSON {
		answer := bytes.TrimSpace([]byte(tally.finalText))
		if len(answer) == 0 || answer[0] != '{' || !json.Valid(answer) {
			return nil, codedError(ErrorCodeAgentOutputInvalid, "the final answer is not exactly one JSON object")
		}
		output.JSON = answer
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return nil, codedError(ErrorCodeAgentOutputInvalid, "encode the agent output: "+err.Error())
	}
	if len(encoded) > maxAgentEnvelopeBytes {
		return nil, codedError(ErrorCodeAgentOutputInvalid, "the agent output exceeds "+strconv.Itoa(maxAgentEnvelopeBytes)+" bytes")
	}
	return encoded, nil
}
