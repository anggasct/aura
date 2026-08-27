package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"log/slog"
	"slices"
	"strings"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/usage"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

// ADKExecutor runs turns through the ADK runner, backed by the Aura session
// and event ports. Every tool invocation is evaluated by the ToolBroker
// before ADK executes it, usage is accumulated across the whole turn so the
// budget applies to retries, fallbacks, and child runs alike, and every ADK
// event is mapped into the Aura runtime event log without losing fidelity.
type ADKExecutor struct {
	appName   string
	modelName string
	sessions  SessionPort
	events    EventStore
	broker    ToolBroker
	tools     []tool.Tool
	logger    *slog.Logger
	builtins  BuiltinToolExecutor
	toolSeq   toolSequence
	publisher EventPublisher
	// ledger, when set with modelDefinitionID, wraps the resolved model with
	// budget enforcement so every turn reserves before dispatch and settles
	// provider-reported usage after.
	ledger            *usage.Ledger
	modelDefinitionID string
}

type EventPublisher interface {
	Publish(*store.RuntimeEvent)
}

// builtinEventPublisher is implemented by builtin tool executors that publish
// tool lifecycle events as they become durable. The executor forwards the
// runtime publisher so tool requests reach the live stream before the
// provider is invoked.
type builtinEventPublisher interface {
	SetEventPublisher(func(*store.RuntimeEvent))
}

func (x *ADKExecutor) SetEventPublisher(publisher EventPublisher) {
	x.publisher = publisher
	if publisher == nil {
		return
	}
	if setter, ok := x.builtins.(builtinEventPublisher); ok {
		setter.SetEventPublisher(publisher.Publish)
	}
}

// ToolBroker is the policy gate every tool call must pass before execution.
// approval.Engine is the canonical implementation.
type ToolBroker interface {
	Evaluate(ctx context.Context, request *approval.ToolRequest) (approval.PolicyDecision, error)
}

// NewADKExecutor builds an ADK-backed turn executor. modelName is resolved
// through the ADK model registry (registered by the model package at
// startup); broker is the tool policy gate; tools are the declared tool set.
// Built-in tools are attached with WithBuiltinToolExecutor. Options attach
// optional capabilities such as budget enforcement.
func NewADKExecutor(appName, modelName string, sessions SessionPort, events EventStore, broker ToolBroker, tools []tool.Tool, logger *slog.Logger, opts ...ExecutorOption) (*ADKExecutor, error) {
	if appName == "" {
		return nil, invalidArgument("app name must not be empty")
	}
	if modelName == "" {
		return nil, invalidArgument("model name must not be empty")
	}
	if sessions == nil {
		return nil, invalidArgument("session port must not be nil")
	}
	if events == nil {
		return nil, invalidArgument("event store must not be nil")
	}
	if broker == nil {
		return nil, invalidArgument("tool broker must not be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	e := &ADKExecutor{
		appName:   appName,
		modelName: modelName,
		sessions:  sessions,
		events:    events,
		broker:    broker,
		tools:     tools,
		logger:    logger,
	}
	for _, opt := range opts {
		if err := opt(e); err != nil {
			return nil, err
		}
	}
	return e, nil
}

// ExecutorOption configures an ADKExecutor at construction.
type ExecutorOption func(*ADKExecutor) error

// WithBudgetLedger wraps the resolved model with the usage budget ledger, so a
// turn reserves a conservative cost before dispatch, settles provider-reported
// usage after, and is rejected before dispatch once the configured cap is
// reached. modelDefinitionID is the pricing key (the config model definition
// name, e.g. "primary").
func WithBudgetLedger(ledger *usage.Ledger, modelDefinitionID string) ExecutorOption {
	return func(e *ADKExecutor) error {
		if ledger == nil {
			return invalidArgument("budget ledger must not be nil")
		}
		if modelDefinitionID == "" {
			return invalidArgument("model definition id must not be empty")
		}
		e.ledger = ledger
		e.modelDefinitionID = modelDefinitionID
		return nil
	}
}

func WithBuiltinToolExecutor(executor BuiltinToolExecutor) ExecutorOption {
	return func(e *ADKExecutor) error {
		if executor == nil {
			return invalidArgument("builtin tool executor must not be nil")
		}
		definitions := cloneBuiltinDefinitions(executor.Definitions())
		tools, err := buildBuiltinTools(definitions)
		if err != nil {
			return err
		}
		e.builtins = executor
		e.tools = tools
		return nil
	}
}

// Execute runs one turn through the ADK runner. The returned events carry
// full ADK fidelity (invocation, branch, author, actions, usage); the engine
// stamps sequence and persists them.
func (x *ADKExecutor) Execute(ctx context.Context, req *TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
	return func(yield func(store.RuntimeEvent, error) bool) {
		runCtx := withTurnID(ctx, req.TurnID)
		sessionService, err := NewADKSessionService(x.sessions)
		if err != nil {
			yield(store.RuntimeEvent{}, err)
			return
		}
		adkRunner, err := x.buildRunner(runCtx, sessionService)
		if err != nil {
			yield(store.RuntimeEvent{}, err)
			return
		}

		content, err := contentFromParts(req)
		if err != nil {
			yield(store.RuntimeEvent{}, err)
			return
		}

		turnUsage := &usageTracker{maxTokens: req.Budget.MaxTokens}
		// WithYieldUserMessage surfaces the user's input as an event so the
		// engine persists it through the same single-writer path; otherwise
		// the user message would live only in the ADK session service, which
		// no longer writes.
		runOpts := []runner.RunOption{runner.WithYieldUserMessage()}
		for ev, err := range adkRunner.Run(runCtx, req.PrincipalID, req.SessionID, content, agent.RunConfig{}, runOpts...) {
			if err != nil {
				if turnUsage.exceeded {
					yield(store.RuntimeEvent{}, codedError(ErrorCodeBudgetExhausted, "turn budget exhausted", nil))
					return
				}
				yield(store.RuntimeEvent{}, err)
				return
			}
			if exceeded := turnUsage.add(ev); exceeded {
				yield(store.RuntimeEvent{}, codedError(ErrorCodeBudgetExhausted, "turn budget exhausted", nil))
				return
			}
			re, err := store.RuntimeEventFromADK(req.SessionID, req.TurnID, ev)
			if err != nil {
				yield(store.RuntimeEvent{}, fmt.Errorf("map adk event: %w", err))
				return
			}
			if !yield(re, nil) {
				return
			}
		}
	}
}

// buildRunner constructs an ADK runner with a single-agent tree over the
// registered model, the Aura-backed session service, and a tool gate that
// evaluates every tool call through the broker before execution.
func (x *ADKExecutor) buildRunner(ctx context.Context, sessionService session.Service) (*runner.Runner, error) {
	model, err := adkmodel.NewLLM(ctx, x.modelName)
	if err != nil {
		return nil, fmt.Errorf("resolve model %q: %w", x.modelName, err)
	}
	if x.ledger != nil {
		wrapped, werr := usage.NewBudgeted(model, x.ledger, x.modelDefinitionID, x.logger)
		if werr != nil {
			return nil, fmt.Errorf("wrap model with budget: %w", werr)
		}
		model = wrapped
	}
	rootAgent, err := buildAgent(x.appName, model, x.tools, x.beforeTool)
	if err != nil {
		return nil, err
	}
	r, err := runner.New(runner.Config{
		AppName:           x.appName,
		Agent:             rootAgent,
		SessionService:    sessionService,
		AutoCreateSession: false,
	})
	if err != nil {
		return nil, fmt.Errorf("build adk runner: %w", err)
	}
	return r, nil
}

// toolGate evaluates one tool invocation through the broker with the full
// identity the security contract requires — principal, session, and
// invocation — so policy can scope per principal and session, and the audit
// trail carries identity. A deny or an evaluation error blocks the call with
// a stable code.
func (x *ADKExecutor) toolGate(actx agent.Context, toolName string, args map[string]any) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return codedError(ErrorCodeRuntimeInternal, "failed to marshal tool arguments", err)
	}
	decision, err := x.broker.Evaluate(actx, &approval.ToolRequest{
		RequestID:   actx.InvocationID(),
		TurnID:      actx.InvocationID(),
		SessionID:   actx.SessionID(),
		PrincipalID: actx.UserID(),
		ToolName:    toolName,
		Arguments:   raw,
		Trust:       approval.TrustDerivedUntrusted,
	})
	if err != nil {
		return codedError(ErrorCodePolicyDenied, "tool evaluation failed closed", err)
	}
	if decision.Outcome != "allow" {
		return codedError(ErrorCodePolicyDenied, "tool call denied by policy", nil)
	}
	return nil
}

func (x *ADKExecutor) executeBuiltinTool(actx agent.Context, toolName string, args map[string]any) (map[string]any, error) {
	definitions := x.builtins.Definitions()
	var definition *BuiltinToolDefinition
	for i := range definitions {
		if definitions[i].Name == toolName {
			definition = &definitions[i]
			break
		}
	}
	if definition == nil {
		return nil, invalidArgument(fmt.Sprintf("unknown builtin tool %q", toolName))
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return nil, codedError(ErrorCodeRuntimeInternal, "failed to marshal tool arguments", err)
	}
	requestID := actx.FunctionCallID()
	if requestID == "" {
		requestID = actx.InvocationID()
	}
	turnID := turnIDFromContext(actx)
	if turnID == "" {
		turnID = actx.InvocationID()
	}
	deadline, _ := actx.Deadline()
	request := &BuiltinToolRequest{
		RequestID:       requestID,
		TurnID:          turnID,
		SessionID:       actx.SessionID(),
		PrincipalID:     actx.UserID(),
		ToolName:        definition.Name,
		ToolVersion:     definition.Version,
		Arguments:       raw,
		Capabilities:    slices.Clone(definition.RequiredCapabilities),
		Trust:           "derived_untrusted",
		Deadline:        deadline,
		IdempotencyKey:  "adk/" + actx.SessionID() + "/" + turnID + "/" + requestID,
		EventInvocation: actx.InvocationID(),
		EventBranch:     actx.Branch(),
		EventAuthor:     actx.AgentName(),
	}
	x.toolSeq.mu.Lock()
	defer x.toolSeq.mu.Unlock()
	sequence, err := x.events.LastSequence(actx, request.SessionID)
	if err != nil {
		return nil, fmt.Errorf("read tool event sequence: %w", err)
	}
	request.EventSequence = sequence + 1
	// Tool lifecycle events are published by the builtin executor as they
	// become durable — before the provider is invoked — so a long-running
	// tool reports progress while it runs. See SetEventPublisher.
	output, executeErr := x.builtins.Execute(actx, request)
	err = executeErr
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		return nil, fmt.Errorf("decode builtin tool result: %w", err)
	}
	return result, nil
}

func contentFromParts(req *TurnRequest) (*genai.Content, error) {
	var parts []*genai.Part
	for _, p := range req.Parts {
		if strings.TrimSpace(p.Text) == "" {
			continue
		}
		parts = append(parts, &genai.Part{Text: p.Text})
	}
	if len(parts) == 0 {
		return nil, invalidArgument("turn has no input parts")
	}
	return &genai.Content{Parts: parts, Role: genai.RoleUser}, nil
}

// usageTracker accumulates token usage across a whole turn so the budget
// binds retries, fallbacks, and child runs alike.
type usageTracker struct {
	maxTokens int64
	exceeded  bool
	tokens    int64
}

// add accounts one ADK event's usage and reports whether the budget is now
// exceeded.
func (u *usageTracker) add(ev *session.Event) bool {
	if ev == nil || ev.UsageMetadata == nil {
		return u.exceeded
	}
	u.tokens += int64(ev.UsageMetadata.TotalTokenCount)
	if u.maxTokens > 0 && u.tokens > u.maxTokens {
		u.exceeded = true
	}
	return u.exceeded
}

func buildAgent(name string, model adkmodel.LLM, tools []tool.Tool, gate llmagent.BeforeToolCallback) (agent.Agent, error) {
	return llmagent.New(llmagent.Config{
		Name:  name,
		Model: model,
		Tools: tools,
		BeforeToolCallbacks: []llmagent.BeforeToolCallback{
			gate,
		},
	})
}

// beforeTool is the llmagent BeforeToolCallback: every tool invocation
// passes through the broker's Evaluate before the tool runs. A deny or an
// evaluation error blocks the call with a stable code.
func (x *ADKExecutor) beforeTool(actx agent.Context, t tool.Tool, args map[string]any) (map[string]any, error) {
	if x.builtins != nil {
		return x.executeBuiltinTool(actx, t.Name(), args)
	}
	if err := x.toolGate(actx, t.Name(), args); err != nil {
		return nil, err
	}
	return args, nil
}
