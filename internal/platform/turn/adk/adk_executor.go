package runtimeadk

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"log/slog"
	"slices"
	"strings"
	"time"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/durable"
	auramodel "github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolcall"
	auratools "github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/platform/turn/store"
	"github.com/anggasct/aura/internal/platform/usage"
	"github.com/anggasct/aura/internal/platform/vision"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/platform"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

type ModelSource interface {
	LLM(ctx context.Context, name string) (adkmodel.LLM, error)
}

type ADKExecutor struct {
	appName           string
	modelName         string
	models            ModelSource
	sessions          SessionPort
	broker            turn.ToolBroker
	tools             []tool.Tool
	operations        *OperationPort
	optIn             map[string]bool
	ownerOptIn        map[string]bool
	channelTools      map[string]string
	externalServers   map[string]string
	pluginTools       map[string]string
	logger            *slog.Logger
	builtins          BuiltinToolExecutor
	agents            AgentResolver
	modelForRoute     func(route string) (string, error)
	ledger            *usage.Ledger
	modelDefinitionID string
	ownerContext      string
	providers         []turn.ContextProvider
	toolBudget        ToolBudget
	clock             func() time.Time
}

type AgentResolver interface {
	Resolve(required []string, preferID *string) (auraagent.Definition, error)
}

func NewADKExecutor(appName, modelName string, models ModelSource, sessions SessionPort, broker turn.ToolBroker, tools []tool.Tool, logger *slog.Logger, opts ...ExecutorOption) (*ADKExecutor, error) {
	if appName == "" {
		return nil, invalidArgument("app name must not be empty")
	}
	if modelName == "" {
		return nil, invalidArgument("model name must not be empty")
	}
	if models == nil {
		return nil, invalidArgument("model source must not be nil")
	}
	if sessions == nil {
		return nil, invalidArgument("session port must not be nil")
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
		models:    models,
		sessions:  sessions,
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
		return e.loadBuiltins(executor)
	}
}

func (x *ADKExecutor) loadBuiltins(executor BuiltinToolExecutor) error {
	if executor == nil {
		return invalidArgument("builtin tool executor must not be nil")
	}
	all := cloneBuiltinDefinitions(executor.Definitions())
	if len(all) == 0 {
		return invalidArgument("builtin tool definitions must not be empty")
	}
	definitions := slices.DeleteFunc(all, func(d BuiltinToolDefinition) bool { return isOperationDefinition(&d) })
	var tools []tool.Tool
	if len(definitions) > 0 {
		built, err := buildBuiltinTools(definitions)
		if err != nil {
			return err
		}
		tools = built
	}
	x.builtins = executor
	x.tools = tools
	x.optIn = map[string]bool{}
	x.ownerOptIn = map[string]bool{}
	x.channelTools = map[string]string{}
	x.externalServers = map[string]string{}
	x.pluginTools = map[string]string{}
	for i := range definitions {
		if definitions[i].OptIn {
			x.optIn[definitions[i].Name] = true
		}
		if definitions[i].OwnerOptIn {
			x.ownerOptIn[definitions[i].Name] = true
		}
		if definitions[i].Channel != "" {
			x.channelTools[definitions[i].Name] = definitions[i].Channel
		}
		if definitions[i].Server != "" {
			x.externalServers[definitions[i].Name] = definitions[i].Server
		}
		if definitions[i].Plugin != "" {
			x.pluginTools[definitions[i].Name] = definitions[i].Plugin
		}
	}
	return nil
}

func WithOwnerContext(block string) ExecutorOption {
	return func(e *ADKExecutor) error {
		e.ownerContext = strings.TrimSpace(block)
		return nil
	}
}

func WithContextProvider(provider turn.ContextProvider) ExecutorOption {
	return func(e *ADKExecutor) error {
		if provider == nil {
			return invalidArgument("context provider must not be nil")
		}
		e.providers = append(e.providers, provider)
		return nil
	}
}

func (x *ADKExecutor) ToolBudget() ToolBudget {
	return x.toolBudget
}

func (x *ADKExecutor) now() time.Time {
	if x.clock != nil {
		return x.clock().UTC()
	}
	return time.Now().UTC()
}

func (x *ADKExecutor) Execute(ctx context.Context, req *turn.TurnRequest) iter.Seq2[turn.Event, error] {
	return func(yield func(turn.Event, error) bool) {
		definition, err := x.resolveDefinition(req)
		if err != nil {
			yield(turn.Event{}, err)
			return
		}
		if err := x.checkPersonaPin(req, &definition); err != nil {
			yield(turn.Event{}, err)
			return
		}
		runCtx := x.withTurnAccess(withTurnID(ctx, req.TurnID), req, definition.ID)
		if req.SessionID != "" {
			runCtx = auramodel.WithSessionID(runCtx, req.SessionID)
		}
		runCtx = platform.WithTaskRunner(runCtx, newToolCallRunner().run)
		if definition.Limits.TurnTimeout > 0 {
			var cancel context.CancelFunc
			runCtx, cancel = context.WithTimeout(runCtx, definition.Limits.TurnTimeout)
			defer cancel()
		}
		toolBudget := newTurnToolBudget(runCtx, x.toolBudget, req.TurnID, x.now, x.logger)
		runCtx = withTurnToolBudget(runCtx, toolBudget)
		sessionService, err := NewADKSessionService(x.sessions)
		if err != nil {
			yield(turn.Event{}, err)
			return
		}
		sessionService.historyAuthor = x.agentName(&definition)
		if _, ok := durable.TurnScopeFrom(runCtx); ok {
			sessionService.leaveOutTurn = req.TurnID
		}
		recallAttached := req.UntrustedContext != nil && !req.WebOnly
		var blocks []turn.ContextBlock
		if !req.WebOnly {
			blocks, err = x.collectContextBlocks(runCtx, req)
			if err != nil {
				yield(turn.Event{}, err)
				return
			}
		}
		extra := turnContext{
			blocks:         blocks,
			knowledgeScope: req.Knowledge,
			restriction:    req.ToolRestriction,
			history:        sessionService,
		}
		if req.WebOnly {
			extra.knowledgeScope = nil
			extra.restriction = turn.WebOnlyRestriction()
			extra.webOnly = webOnlyInstruction(x.now(), req.Timezone)
		}
		extra.plan = x.planOperations(runCtx, &planInput{definition: &definition, req: req, extra: &extra})
		runCtx = withOperationPlan(runCtx, extra.plan)
		adkRunner, err := x.buildRunner(runCtx, sessionService, &definition, recallAttached, x.ownerContextFor(req), &extra)
		if err != nil {
			yield(turn.Event{}, err)
			return
		}
		for i := range blocks {
			for j := range blocks[i].Events {
				if !yield(blocks[i].Events[j], nil) {
					return
				}
			}
		}

		var userParts []string
		for i := range blocks {
			if blocks[i].Placement == turn.ContextPlacementUserPart {
				if text := strings.TrimSpace(blocks[i].Content); text != "" {
					userParts = append(userParts, text)
				}
			}
		}
		content, err := contentFromParts(req, userParts...)
		if err != nil {
			yield(turn.Event{}, err)
			return
		}

		turnUsage := &usageTracker{maxTokens: req.Budget.MaxTokens}
		runOpts := []runner.RunOption{runner.WithYieldUserMessage()}
		runConfig := agent.RunConfig{}
		if req.StreamPartial {
			runConfig.StreamingMode = agent.StreamingModeSSE
		}
		for ev, err := range adkRunner.Run(runCtx, req.PrincipalID, req.SessionID, content, runConfig, runOpts...) {
			if err != nil {
				if turnUsage.exceeded {
					yield(turn.Event{}, codedError(turn.ErrorCodeBudgetExhausted, "turn budget exhausted", nil))
					return
				}
				yield(turn.Event{}, err)
				return
			}
			if exceeded := turnUsage.add(ev); exceeded {
				yield(turn.Event{}, codedError(turn.ErrorCodeBudgetExhausted, "turn budget exhausted", nil))
				return
			}
			if budgetErr := toolBudget.observeEvent(ev, req.Origin); budgetErr != nil {
				yield(turn.Event{}, budgetErr)
				return
			}
			re, err := turnstore.RuntimeEventFromADK(req.SessionID, req.TurnID, ev)
			if err != nil {
				yield(turn.Event{}, fmt.Errorf("map adk event: %w", err))
				return
			}
			if !yield(re, nil) {
				return
			}
		}
	}
}

func (x *ADKExecutor) resolveDefinition(req *turn.TurnRequest) (auraagent.Definition, error) {
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

func (x *ADKExecutor) checkPersonaPin(req *turn.TurnRequest, definition *auraagent.Definition) error {
	pin := req.Persona
	if pin == nil || x.agents == nil || req.Origin != turn.OriginWorkflow {
		return nil
	}
	current := definition.Fingerprint()
	if current.SoulSHA256 != pin.SoulSHA256 || current.ModelRoute != pin.ModelRoute {
		return codedError(turn.ErrorCodePersonaMismatch, fmt.Sprintf("persona %q changed since it was pinned", definition.ID), nil)
	}
	return nil
}

func (x *ADKExecutor) ownerContextFor(req *turn.TurnRequest) string {
	if req.PrincipalClass != principal.ClassOwner {
		return ""
	}
	return x.ownerContext
}

type turnContext struct {
	blocks         []turn.ContextBlock
	knowledgeScope []string
	restriction    *turn.ToolRestriction
	history        historyReader
	plan           *operationPlan
	webOnly        string
}

func (x *ADKExecutor) collectContextBlocks(ctx context.Context, req *turn.TurnRequest) ([]turn.ContextBlock, error) {
	var blocks []turn.ContextBlock
	for _, provider := range x.providers {
		provided, err := provider.TurnContext(ctx, req)
		if err != nil {
			if provider.Required(req) {
				return nil, codedError(turn.ErrorCodeStorageUnavailable, "required context provider is unavailable", err)
			}
			x.logger.WarnContext(ctx, "context provider unavailable", "component", "turn", "error", err, "turn_id", req.TurnID)
			continue
		}
		blocks = append(blocks, provided...)
	}
	slices.SortStableFunc(blocks, func(a, b turn.ContextBlock) int {
		return cmp.Compare(a.Order, b.Order)
	})
	return blocks, nil
}

const userAuthor = "user"

func (x *ADKExecutor) agentName(definition *auraagent.Definition) string {
	return cmp.Or(definition.Name, x.appName)
}

func (x *ADKExecutor) buildRunner(ctx context.Context, sessionService session.Service, definition *auraagent.Definition, recallAttached bool, ownerContext string, extra *turnContext) (*runner.Runner, error) {
	model, err := x.resolveModel(ctx, definition)
	if err != nil {
		return nil, err
	}
	if x.ledger != nil {
		access := turnAccessFromContext(ctx)
		wrapped, werr := usage.NewBudgeted(model, x.ledger, x.modelDefinitionID, usage.Attribution{
			AgentID:        definition.ID,
			PrincipalID:    access.principalID,
			PrincipalClass: string(access.class),
		}, x.logger)
		if werr != nil {
			return nil, fmt.Errorf("wrap model with budget: %w", werr)
		}
		model = wrapped
	}
	model = nameCalls(model)
	if _, ok := durable.TurnScopeFrom(ctx); ok {
		model = journalModel(model)
	}
	rootAgent, err := buildAgent(x.agentName(definition), definition, model, extra.plan.core, x.beforeTool, recallAttached, ownerContext, extra)
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

func canonicalToolArguments(args map[string]any) (json.RawMessage, error) {
	if args == nil {
		args = map[string]any{}
	}
	return json.Marshal(args)
}

func (x *ADKExecutor) toolGate(callCtx context.Context, actx agent.Context, toolName string, args map[string]any) error {
	raw, err := canonicalToolArguments(args)
	if err != nil {
		return codedError(turn.ErrorCodeRuntimeInternal, "failed to marshal tool arguments", err)
	}
	decision, err := x.broker.Evaluate(callCtx, &approval.ToolRequest{
		RequestID:   actx.InvocationID(),
		TurnID:      actx.InvocationID(),
		SessionID:   actx.SessionID(),
		PrincipalID: actx.UserID(),
		ToolName:    toolName,
		Arguments:   raw,
		Trust:       approval.TrustDerivedUntrusted,
	})
	if err != nil {
		return codedError(turn.ErrorCodePolicyDenied, "tool evaluation failed closed", err)
	}
	if decision.Outcome != "allow" {
		return codedError(turn.ErrorCodePolicyDenied, "tool call denied by policy", nil)
	}
	return nil
}

func (x *ADKExecutor) executeBuiltinTool(callCtx context.Context, actx agent.Context, toolName string, args map[string]any, callDeadline time.Time) (map[string]any, error) {
	builtins := x.builtinsFor(toolName)
	if builtins == nil {
		return nil, invalidArgument(fmt.Sprintf("unknown builtin tool %q", toolName))
	}
	definitions := builtins.Definitions()
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
	raw, err := canonicalToolArguments(args)
	if err != nil {
		return nil, codedError(turn.ErrorCodeRuntimeInternal, "failed to marshal tool arguments", err)
	}
	slot, ok := toolcall.From(callCtx)
	if !ok {
		return nil, invalidArgument(fmt.Sprintf("tool %q has no call key", toolName))
	}
	requestID := actx.FunctionCallID()
	if requestID == "" {
		requestID = actx.InvocationID()
	}
	turnID := turnIDFromContext(callCtx)
	if turnID == "" {
		turnID = actx.InvocationID()
	}
	deadline := callDeadline
	if deadline.IsZero() {
		deadline, _ = callCtx.Deadline()
	}
	access := turnAccessFromContext(callCtx)
	if toolName == auratools.OperationSearchToolName {
		scoped, err := x.withSearchScope(callCtx, actx.SessionID(), &access)
		if err != nil {
			return nil, err
		}
		callCtx = scoped
	}
	request := &BuiltinToolRequest{
		RequestID:       requestID,
		TurnID:          turnID,
		SessionID:       actx.SessionID(),
		PrincipalID:     actx.UserID(),
		PrincipalClass:  access.class,
		AgentID:         access.agentID,
		Knowledge:       slices.Clone(access.knowledge),
		Tainted:         access.tainted,
		TaintSource:     access.taintSource,
		Origin:          access.origin,
		Unattended:      access.unattended && x.generatesOperation(definition.Name),
		WebOnlyRelief:   access.webOnlyRelief,
		ToolName:        definition.Name,
		ToolVersion:     definition.Version,
		Arguments:       raw,
		Capabilities:    slices.Clone(definition.RequiredCapabilities),
		Trust:           "derived_untrusted",
		Deadline:        deadline,
		IdempotencyKey:  "adk/" + actx.SessionID() + "/" + turnID + "/" + slot.Key(),
		EventInvocation: actx.InvocationID(),
		EventBranch:     actx.Branch(),
		EventAuthor:     actx.AgentName(),

		ContentTaintTracked: access.contentTaintTracked,
	}
	output, err := builtins.Execute(callCtx, request)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		return nil, fmt.Errorf("decode builtin tool result: %w", err)
	}
	return result, nil
}

func contentFromParts(req *turn.TurnRequest, extraParts ...string) (*genai.Content, error) {
	var parts []*genai.Part
	for _, p := range req.Parts {
		if len(p.Image) > 0 {
			imageParts, err := imageContentParts(p.Image)
			if err != nil {
				return nil, err
			}
			parts = append(parts, imageParts...)
			continue
		}
		if strings.TrimSpace(p.Text) == "" {
			continue
		}
		parts = append(parts, &genai.Part{Text: p.Text})
	}
	if block := turn.RenderUntrustedRecall(req.UntrustedContext); strings.TrimSpace(block) != "" {
		parts = append(parts, &genai.Part{Text: block})
	}
	for _, extra := range extraParts {
		if strings.TrimSpace(extra) != "" {
			parts = append(parts, &genai.Part{Text: extra})
		}
	}
	if len(parts) == 0 {
		return nil, invalidArgument("turn has no input parts")
	}
	return &genai.Content{Parts: parts, Role: genai.RoleUser}, nil
}

func imageContentParts(envelope json.RawMessage) ([]*genai.Part, error) {
	part, err := vision.UnmarshalPart(envelope)
	if err != nil {
		return nil, invalidArgument("turn image reference is invalid")
	}
	out := []*genai.Part{{
		FileData: &genai.FileData{
			FileURI:  "artifact://" + part.ArtifactID,
			MIMEType: part.MIME,
		},
		PartMetadata: map[string]any{vision.PartMetadataKey: string(envelope)},
	}}
	if alt := strings.TrimSpace(part.AltText); alt != "" {
		out = append(out, &genai.Part{Text: alt})
	}
	return out, nil
}

type usageTracker struct {
	maxTokens int64
	exceeded  bool
	tokens    int64
}

func (u *usageTracker) add(ev *session.Event) bool {
	if ev == nil || ev.Partial || ev.UsageMetadata == nil {
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
	model, err := x.models.LLM(ctx, modelName)
	if err != nil {
		return nil, fmt.Errorf("resolve model %q: %w", modelName, err)
	}
	return model, nil
}

func (x *ADKExecutor) toolsFor(definition *auraagent.Definition, class principal.Class, origin turn.Origin, extra *turnContext) []tool.Tool {
	if extra.webOnly != "" {
		return x.webOnlyTools(extra.restriction)
	}
	allowed, ok := x.allowedTools(definition, class)
	if !ok {
		return nil
	}
	listed := len(allowed) > 0
	filtered := make([]tool.Tool, 0, len(x.tools))
	for _, t := range x.tools {
		if extra.restriction != nil && !slices.Contains(extra.restriction.Allowed, t.Name()) {
			continue
		}
		if plugin, owned := x.pluginTools[t.Name()]; owned {
			if exposesPluginTool(definition, class, t.Name(), plugin) {
				filtered = append(filtered, t)
			}
			continue
		}
		if server, external := x.externalServers[t.Name()]; external {
			if exposesExternalTool(definition, class, t.Name(), server) {
				filtered = append(filtered, t)
			}
			continue
		}
		if t.Name() == auratools.KBSearchToolName {
			if exposesKnowledgeSearch(class, allowed, extra.knowledgeScope) {
				filtered = append(filtered, t)
			}
			continue
		}
		channel := x.channelTools[t.Name()]
		if channel != "" && !originOnChannel(origin, channel) {
			continue
		}
		if x.optIn[t.Name()] && (!listed || class == "" || (class == principal.ClassOwner && channel == "" && !x.ownerListed(t.Name(), origin))) {
			continue
		}
		if !listed || slices.Contains(allowed, t.Name()) {
			filtered = append(filtered, t)
		}
	}
	return append(filtered, x.listedOperationTools(definition, class, origin, extra, allowed)...)
}

func (x *ADKExecutor) ownerListed(name string, origin turn.Origin) bool {
	return x.ownerOptIn[name] && origin != turn.OriginChild && origin != turn.OriginWorkflow
}

func exposesKnowledgeSearch(class principal.Class, allowed, knowledge []string) bool {
	if len(knowledge) == 0 {
		return false
	}
	return class == principal.ClassOwner || slices.Contains(allowed, auratools.KBSearchToolName)
}

func originOnChannel(origin turn.Origin, channel string) bool {
	head, _, found := strings.Cut(string(origin), ":")
	return found && head == channel
}

func exposesExternalTool(definition *auraagent.Definition, class principal.Class, toolName, server string) bool {
	if class == principal.ClassPublic {
		return false
	}
	allowed := definition.Tools
	if class != "" {
		if ceiling, restricted := definition.ToolCeiling(class); restricted {
			allowed = ceiling
		}
	}
	return auratools.ListsTool(allowed, toolName, server)
}

func exposesPluginTool(definition *auraagent.Definition, class principal.Class, toolName, plugin string) bool {
	if class == principal.ClassPublic || !definition.ListsPlugin(plugin) {
		return false
	}
	if class == "" {
		return true
	}
	ceiling, restricted := definition.ToolCeiling(class)
	return !restricted || auratools.ListsPluginTool(ceiling, toolName, plugin)
}

func buildAgent(name string, definition *auraagent.Definition, model adkmodel.LLM, tools []tool.Tool, gate llmagent.BeforeToolCallback, recallAttached bool, ownerContext string, extra *turnContext) (agent.Agent, error) {
	instruction := definition.Instructions
	if extra.webOnly != "" {
		instruction = extra.webOnly
	} else if ownerContext != "" {
		instruction = ownerContext
		if soul := strings.TrimSpace(definition.Instructions); soul != "" {
			instruction += "\n\n" + soul
		}
	}
	if extra.plan != nil && extra.plan.description != "" {
		instruction = strings.TrimSpace(instruction + "\n\n" + extra.plan.description)
	}
	if recallAttached {
		instruction = strings.TrimSpace(instruction) + "\n\n" + turn.RecallEvidenceCaveat
	}
	var toolsets []tool.Toolset
	if extra.plan != nil && extra.plan.toolset != nil {
		toolsets = append(toolsets, extra.plan.toolset)
	}
	var beforeModel []llmagent.BeforeModelCallback
	for i := range extra.blocks {
		if caveat := strings.TrimSpace(extra.blocks[i].Caveat); caveat != "" && (!recallAttached || caveat != turn.RecallEvidenceCaveat) {
			instruction = strings.TrimSpace(instruction) + "\n\n" + caveat
		}
		if extra.blocks[i].Placement == turn.ContextPlacementEphemeral {
			if content := strings.TrimSpace(extra.blocks[i].Content); content != "" {
				beforeModel = append(beforeModel, withUntrustedContextPart(content))
			}
		}
	}
	instruction = appendTurnProtocol(instruction)
	return llmagent.New(llmagent.Config{
		Name:                 name,
		Description:          definition.Description,
		Instruction:          instruction,
		Model:                model,
		Tools:                tools,
		Toolsets:             toolsets,
		BeforeModelCallbacks: beforeModel,
		BeforeToolCallbacks: []llmagent.BeforeToolCallback{
			gate,
		},
	})
}

// The part is added per model request, never to session events, so served facts are not journaled, projected into memory, or re-read by the extractor.
func withUntrustedContextPart(block string) llmagent.BeforeModelCallback {
	return func(_ agent.Context, req *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
		part := &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{Text: block}}}
		req.Contents = append([]*genai.Content{part}, req.Contents...)
		return nil, nil
	}
}

func (x *ADKExecutor) beforeTool(actx agent.Context, t tool.Tool, args map[string]any) (map[string]any, error) {
	callCtx := context.Context(actx)
	var callDeadline time.Time
	if budget := turnToolBudgetFromContext(actx); budget != nil {
		slot, _ := toolcall.From(actx)
		decision, err := budget.check(actx, slot, t.Name(), actx.FunctionCallID())
		if err != nil {
			return nil, err
		}
		if !decision.allowed {
			return decision.refusal, nil
		}
		if !decision.softDeadline.IsZero() {
			callDeadline = decision.softDeadline
			if _, scoped := durable.TurnScopeFrom(actx); !scoped || x.builtinsFor(t.Name()) == nil {
				var cancel context.CancelFunc
				callCtx, cancel = context.WithDeadline(actx, decision.softDeadline)
				defer cancel()
			}
		}
	}
	if x.builtinsFor(t.Name()) != nil {
		return x.executeBuiltinTool(callCtx, actx, t.Name(), args, callDeadline)
	}
	if err := x.toolGate(callCtx, actx, t.Name(), args); err != nil {
		return nil, err
	}
	return args, nil
}
