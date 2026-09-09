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
	"github.com/anggasct/aura/internal/durable"
	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/runtime/engine"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/usage"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/platform"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

type ADKExecutor struct {
	appName           string
	modelName         string
	sessions          SessionPort
	events            runtimeengine.EventStore
	broker            runtime.ToolBroker
	tools             []tool.Tool
	logger            *slog.Logger
	builtins          BuiltinToolExecutor
	toolSeq           toolSequence
	publisher         EventPublisher
	agents            AgentResolver
	modelForRoute     func(route string) (string, error)
	ledger            *usage.Ledger
	modelDefinitionID string
}

type AgentResolver interface {
	Resolve(required []string, preferID *string) (auraagent.Definition, error)
}

type EventPublisher interface {
	Publish(*store.RuntimeEvent)
}

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

type ExecutorOption func(*ADKExecutor) error

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

func (x *ADKExecutor) Execute(ctx context.Context, req *runtime.TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
	return func(yield func(store.RuntimeEvent, error) bool) {
		definition, err := x.resolveDefinition(req)
		if err != nil {
			yield(store.RuntimeEvent{}, err)
			return
		}
		runCtx := withTurnID(ctx, req.TurnID)
		if _, ok := durable.TurnScopeFrom(runCtx); ok {
			runCtx = platform.WithTaskRunner(runCtx, runTasksSequential)
		}
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
	if _, ok := durable.TurnScopeFrom(ctx); ok {
		model = journalModel(model)
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

type usageTracker struct {
	maxTokens int64
	exceeded  bool
	tokens    int64
}

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

func (x *ADKExecutor) beforeTool(actx agent.Context, t tool.Tool, args map[string]any) (map[string]any, error) {
	if x.builtins != nil {
		return x.executeBuiltinTool(actx, t.Name(), args)
	}
	if err := x.toolGate(actx, t.Name(), args); err != nil {
		return nil, err
	}
	return args, nil
}
