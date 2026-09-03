package runtimeadk

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"log/slog"
	"slices"
	"strings"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/runtime/engine"
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
// and event ports. Every tool invocation is evaluated by the runtime.ToolBroker
// before ADK executes it, usage is accumulated across the whole turn so the
// budget applies to retries, fallbacks, and child runs alike, and every ADK
// event is mapped into the Aura runtime event log without losing fidelity.
type ADKExecutor struct {
	appName   string
	modelName string
	sessions  SessionPort
	events    runtimeengine.EventStore
	broker    runtime.ToolBroker
	tools     []tool.Tool
	logger    *slog.Logger
	builtins  BuiltinToolExecutor
	toolSeq   toolSequence
	publisher EventPublisher
	// agents, when set, resolves the target definition for every turn and
	// drives the ADK agent construction from that definition; modelForRoute
	// maps a definition's model route onto a registered model name.
	agents        AgentResolver
	modelForRoute func(route string) (string, error)
	// ledger, when set with modelDefinitionID, wraps the resolved model with
	// budget enforcement so every turn reserves before dispatch and settles
	// provider-reported usage after.
	ledger            *usage.Ledger
	modelDefinitionID string
}

// AgentResolver selects the definition a turn runs on. The registry in the
// agent package is the canonical implementation; the interface is declared
// here so the executor depends only on the resolution it performs.
type AgentResolver interface {
	Resolve(required []string, preferID *string) (auraagent.Definition, error)
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

// NewADKExecutor builds an ADK-backed turn executor. modelName is resolved
// through the ADK model registry (registered by the model package at
// startup); broker is the tool policy gate; tools are the declared tool set.
// Built-in tools are attached with WithBuiltinToolExecutor. Options attach
// optional capabilities such as budget enforcement.
func NewADKExecutor(appName, modelName string, sessions SessionPort, events runtimeengine.EventStore, broker runtime.ToolBroker, tools []tool.Tool, logger *slog.Logger, opts ...ExecutorOption) (*ADKExecutor, error) {
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

// WithAgentResolver attaches declarative agent definitions: every turn
// resolves its target definition (req.AgentID when set, else the most
// specific default), and the ADK agent is constructed from that definition's
// instructions, tool subset, and model route. modelForRoute maps a
// definition's model route onto a registered model name.
func WithAgentResolver(resolver AgentResolver, modelForRoute func(route string) (string, error)) ExecutorOption {
	return func(e *ADKExecutor) error {
		if resolver == nil {
			return invalidArgument("agent resolver must not be nil")
		}
		e.agents = resolver
		e.modelForRoute = modelForRoute
		return nil
	}
}

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
func (x *ADKExecutor) Execute(ctx context.Context, req *runtime.TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
	return func(yield func(store.RuntimeEvent, error) bool) {
		definition, err := x.resolveDefinition(req)
		if err != nil {
			yield(store.RuntimeEvent{}, err)
			return
		}
		runCtx := withTurnID(ctx, req.TurnID)
		if definition.Limits.TurnTimeout > 0 {
			var cancel context.CancelFunc
			runCtx, cancel = context.WithTimeout(runCtx, definition.Limits.TurnTimeout)
			defer cancel()
		}
		sessionService, err := NewADKSessionService(x.sessions)
		if err != nil {
			yield(store.RuntimeEvent{}, err)
			return
		}
		adkRunner, err := x.buildRunner(runCtx, sessionService, &definition)
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
					yield(store.RuntimeEvent{}, codedError(runtime.ErrorCodeBudgetExhausted, "turn budget exhausted", nil))
					return
				}
				yield(store.RuntimeEvent{}, err)
				return
			}
			if exceeded := turnUsage.add(ev); exceeded {
				yield(store.RuntimeEvent{}, codedError(runtime.ErrorCodeBudgetExhausted, "turn budget exhausted", nil))
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

// resolveDefinition picks the definition the turn runs on: the requested id
// when the request targets one, else the deterministic default. Resolution
// failure returns before any work starts.
func (x *ADKExecutor) resolveDefinition(req *runtime.TurnRequest) (auraagent.Definition, error) {
	if x.agents == nil {
		return auraagent.Definition{}, nil
	}
	var prefer *string
	if req.AgentID != "" {
		prefer = &req.AgentID
	}
	definition, err := x.agents.Resolve(nil, prefer)
	if err != nil {
		return auraagent.Definition{}, fmt.Errorf("resolve agent definition: %w", err)
	}
	return definition, nil
}

// buildRunner constructs an ADK runner with a single-agent tree over the
// registered model, the Aura-backed session service, and a tool gate that
// evaluates every tool call through the broker before execution.
func (x *ADKExecutor) buildRunner(ctx context.Context, sessionService session.Service, definition *auraagent.Definition) (*runner.Runner, error) {
	model, err := x.resolveModel(ctx, definition)
	if err != nil {
		return nil, err
	}
	if x.ledger != nil {
		wrapped, werr := usage.NewBudgeted(model, x.ledger, x.modelDefinitionID, x.logger)
		if werr != nil {
			return nil, fmt.Errorf("wrap model with budget: %w", werr)
		}
		model = wrapped
	}
	rootAgent, err := buildAgent(x.appName, definition, model, x.toolsFor(definition), x.beforeTool)
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
		return codedError(runtime.ErrorCodeRuntimeInternal, "failed to marshal tool arguments", err)
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
		return codedError(runtime.ErrorCodePolicyDenied, "tool evaluation failed closed", err)
	}
	if decision.Outcome != "allow" {
		return codedError(runtime.ErrorCodePolicyDenied, "tool call denied by policy", nil)
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
		return nil, codedError(runtime.ErrorCodeRuntimeInternal, "failed to marshal tool arguments", err)
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

func contentFromParts(req *runtime.TurnRequest) (*genai.Content, error) {
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

// resolveModel resolves the registered model for the turn: the definition's
// model route when it declares one, else the executor's configured model.
func (x *ADKExecutor) resolveModel(ctx context.Context, definition *auraagent.Definition) (adkmodel.LLM, error) {
	modelName := x.modelName
	if definition.ModelRoute != "" {
		if x.modelForRoute == nil {
			return nil, invalidArgument("agent model route requires a route resolver")
		}
		routeModel, err := x.modelForRoute(definition.ModelRoute)
		if err != nil {
			return nil, fmt.Errorf("resolve model route %q: %w", definition.ModelRoute, err)
		}
		modelName = routeModel
	}
	model, err := adkmodel.NewLLM(ctx, modelName)
	if err != nil {
		return nil, fmt.Errorf("resolve model %q: %w", modelName, err)
	}
	return model, nil
}

// toolsFor narrows the executor tool set to the definition's declared tools;
// a definition without declared tools runs the full set.
func (x *ADKExecutor) toolsFor(definition *auraagent.Definition) []tool.Tool {
	if len(definition.Tools) == 0 {
		return x.tools
	}
	filtered := make([]tool.Tool, 0, len(x.tools))
	for _, t := range x.tools {
		if slices.Contains(definition.Tools, t.Name()) {
			filtered = append(filtered, t)
		}
	}
	return filtered
}

func buildAgent(name string, definition *auraagent.Definition, model adkmodel.LLM, tools []tool.Tool, gate llmagent.BeforeToolCallback) (agent.Agent, error) {
	return llmagent.New(llmagent.Config{
		Name:        name,
		Description: definition.Description,
		Instruction: definition.Instructions,
		Model:       model,
		Tools:       tools,
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
