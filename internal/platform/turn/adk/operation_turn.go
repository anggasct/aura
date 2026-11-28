package runtimeadk

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/principal"
	auratools "github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
)

type OperationSurface interface {
	Describe(input *operation.DescriptionInput) (string, error)
}

type historyReader interface {
	CurrentEvents(sessionID string) ([]*session.Event, error)
}

type operationPlan struct {
	core        []tool.Tool
	kind        operation.TurnKind
	description string
	toolset     tool.Toolset
	history     historyReader
	visible     func(name string) bool
}

type operationPlanKey struct{}

func withOperationPlan(ctx context.Context, plan *operationPlan) context.Context {
	return context.WithValue(ctx, operationPlanKey{}, plan)
}

func operationPlanFrom(ctx context.Context) *operationPlan {
	plan, _ := ctx.Value(operationPlanKey{}).(*operationPlan)
	return plan
}

func (x *ADKExecutor) turnKind(origin turn.Origin) operation.TurnKind {
	return operation.TurnKindOf(origin)
}

func (x *ADKExecutor) binding() *operationBinding {
	if x.operations == nil {
		return nil
	}
	return x.operations.bound.Load()
}

func (x *ADKExecutor) isOperationTool(name string) bool {
	binding := x.binding()
	return binding != nil && binding.owns(name)
}

func (x *ADKExecutor) generatesOperation(name string) bool {
	binding := x.binding()
	return binding != nil && binding.generates(name)
}

func (x *ADKExecutor) builtinsFor(name string) BuiltinToolExecutor {
	if binding := x.binding(); binding != nil && binding.owns(name) {
		return binding.executor
	}
	return x.builtins
}

func (x *ADKExecutor) listedOperationTools(definition *auraagent.Definition, class principal.Class, origin turn.Origin, extra *turnContext, allowed []string) []tool.Tool {
	binding := x.binding()
	if binding == nil || extra.restriction != nil || !x.ownerTurn(definition, class, origin) {
		return nil
	}
	var listed []tool.Tool
	for _, name := range binding.order {
		if slices.Contains(allowed, name) {
			listed = append(listed, binding.tools[name])
		}
	}
	return listed
}

func (x *ADKExecutor) WithholdableTools() []string {
	var names []string
	for _, t := range x.tools {
		name := t.Name()
		_, plugin := x.pluginTools[name]
		_, external := x.externalServers[name]
		_, channel := x.channelTools[name]
		if plugin || external || channel || name == auratools.KBSearchToolName {
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func (x *ADKExecutor) ownerTurn(definition *auraagent.Definition, class principal.Class, origin turn.Origin) bool {
	return class == principal.ClassOwner && definition.Audience == auraagent.AudienceOwner && origin != "" && origin != turn.OriginChild && origin != turn.OriginWorkflow
}

type planInput struct {
	definition *auraagent.Definition
	req        *turn.TurnRequest
	extra      *turnContext
}

func (x *ADKExecutor) planOperations(ctx context.Context, in *planInput) *operationPlan {
	definition, req, extra := in.definition, in.req, in.extra
	plan := &operationPlan{kind: x.turnKind(req.Origin), core: x.toolsFor(definition, req.PrincipalClass, req.Origin, extra)}
	binding := x.binding()
	if binding == nil || extra.restriction != nil || !x.ownerTurn(definition, req.PrincipalClass, req.Origin) {
		return plan
	}
	allowed, ok := definition.ListedTools(req.PrincipalClass)
	if !ok || !slices.ContainsFunc(allowed, auratools.IsOperationEntry) {
		return plan
	}
	tracked := turnAccessFromContext(ctx).contentTaintTracked
	searchOffered := slices.ContainsFunc(plan.core, func(t tool.Tool) bool { return t.Name() == auratools.OperationSearchToolName })
	description, err := binding.surface.Describe(&operation.DescriptionInput{
		Turn:     operation.Turn{Origin: req.Origin, Kind: plan.kind, ContentTaintTracked: tracked},
		Search:   searchOffered,
		Listed:   namesOf(plan.core),
		Withheld: x.withheldTools(plan.core),
	})
	if err != nil {
		x.logger.WarnContext(ctx, "operation description unavailable", "component", "turn", "turn_id", req.TurnID, "error", err)
		plan.core = slices.DeleteFunc(plan.core, func(t tool.Tool) bool { return x.isOperationTool(t.Name()) })
		return plan
	}
	plan.description = description
	if searchOffered && extra.history != nil {
		plan.history = extra.history
		plan.visible = binding.generates
		plan.toolset = &operationToolset{plan: plan, core: plan.core, binding: binding, logger: x.logger, turnID: req.TurnID}
	}
	return plan
}

func (x *ADKExecutor) withheldTools(offered []tool.Tool) []string {
	names := namesOf(offered)
	return slices.DeleteFunc(x.WithholdableTools(), func(name string) bool { return slices.Contains(names, name) })
}

func namesOf(tools []tool.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name())
	}
	return names
}

func (p *operationPlan) loadedNow(sessionID string) ([]string, error) {
	if p == nil || p.history == nil || p.visible == nil {
		return nil, nil
	}
	events, err := p.history.CurrentEvents(sessionID)
	if err != nil {
		return nil, err
	}
	return recordedLoads(events, p.visible), nil
}

func (p *operationPlan) offeredNames() []string {
	names := namesOf(p.core)
	slices.Sort(names)
	return names
}

func recordedLoads(events []*session.Event, visible func(name string) bool) []string {
	calls := map[string]struct{}{}
	var loaded []string
	for _, ev := range events {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, part := range ev.Content.Parts {
			switch {
			case part == nil:
			case part.FunctionCall != nil && ev.Author != userAuthor && part.FunctionCall.Name == auratools.OperationSearchToolName && strings.HasPrefix(part.FunctionCall.ID, callIDPrefix):
				calls[part.FunctionCall.ID] = struct{}{}
			case part.FunctionResponse != nil && part.FunctionResponse.Name == auratools.OperationSearchToolName:
				if _, recorded := calls[part.FunctionResponse.ID]; !recorded {
					continue
				}
				for _, name := range operation.RecordedLoads(part.FunctionResponse.Response) {
					if visible(name) && !slices.Contains(loaded, name) {
						loaded = append(loaded, name)
						if len(loaded) >= operation.MaxLoaded {
							return loaded
						}
					}
				}
			}
		}
	}
	return loaded
}

type operationToolset struct {
	plan    *operationPlan
	core    []tool.Tool
	binding *operationBinding
	logger  *slog.Logger
	turnID  string
}

func (s *operationToolset) Name() string { return "aura_operations" }

func (s *operationToolset) Tools(ctx agent.ReadonlyContext) ([]tool.Tool, error) {
	names, err := s.plan.loadedNow(ctx.SessionID())
	if err != nil {
		s.logger.WarnContext(ctx, "operation toolset unavailable", "component", "turn", "turn_id", s.turnID, "error", fmt.Errorf("read the session history: %w", err))
		return nil, nil
	}
	loaded := make([]tool.Tool, 0, len(names))
	for _, name := range names {
		if slices.ContainsFunc(s.core, func(t tool.Tool) bool { return t.Name() == name }) {
			continue
		}
		if found, ok := s.binding.tools[name]; ok {
			loaded = append(loaded, found)
		}
	}
	return loaded, nil
}

func (x *ADKExecutor) withSearchScope(ctx context.Context, sessionID string, access *turnAccess) (context.Context, error) {
	plan := operationPlanFrom(ctx)
	if plan == nil || plan.toolset == nil {
		return nil, codedError(turn.ErrorCodePolicyDenied, "the discovery tool is not offered on this turn", nil)
	}
	loaded, err := plan.loadedNow(sessionID)
	if err != nil {
		return nil, codedError(turn.ErrorCodeRuntimeInternal, "the loaded operations cannot be read", err)
	}
	return operation.WithSearchScope(ctx, &operation.SearchScope{
		Kind:                plan.kind,
		ContentTaintTracked: access.contentTaintTracked,
		Offered:             plan.offeredNames(),
		Loaded:              loaded,
	}), nil
}
