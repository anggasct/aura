package runtimeadk

import (
	"context"
	"fmt"
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
	return operation.TurnKindOf(origin, !x.unattendedTerminal)
}

func (x *ADKExecutor) isOperationTool(name string) bool {
	return x.operationTools[name] || name == auratools.OperationSearchToolName
}

func (x *ADKExecutor) ownerTurn(definition *auraagent.Definition, class principal.Class, origin turn.Origin) bool {
	return class == principal.ClassOwner && definition.Audience == auraagent.AudienceOwner && origin != "" && origin != turn.OriginChild && origin != turn.OriginWorkflow
}

func (x *ADKExecutor) allowedTools(definition *auraagent.Definition, class principal.Class) ([]string, bool) {
	allowed := definition.Tools
	if class != "" {
		ceiling, restricted := definition.ToolCeiling(class)
		if restricted {
			allowed = ceiling
			if len(allowed) == 0 {
				return nil, false
			}
		}
	}
	return allowed, true
}

type planInput struct {
	definition *auraagent.Definition
	req        *turn.TurnRequest
	extra      *turnContext
}

func (x *ADKExecutor) planOperations(ctx context.Context, in *planInput) *operationPlan {
	definition, req, extra := in.definition, in.req, in.extra
	plan := &operationPlan{kind: x.turnKind(req.Origin), core: x.toolsFor(definition, req.PrincipalClass, req.Origin, extra)}
	if x.operations == nil || extra.restriction != nil || !x.ownerTurn(definition, req.PrincipalClass, req.Origin) {
		return plan
	}
	allowed, ok := x.allowedTools(definition, req.PrincipalClass)
	if !ok || !slices.ContainsFunc(allowed, auratools.IsOperationEntry) {
		return plan
	}
	tracked := turnAccessFromContext(ctx).contentTaintTracked
	searchOffered := slices.ContainsFunc(plan.core, func(t tool.Tool) bool { return t.Name() == auratools.OperationSearchToolName })
	description, err := x.operations.Describe(&operation.DescriptionInput{
		Turn:     operation.Turn{Origin: req.Origin, Kind: plan.kind, ContentTaintTracked: tracked},
		Search:   searchOffered,
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
		plan.visible = func(name string) bool { return x.operationTools[name] }
		plan.toolset = &operationToolset{plan: plan, core: plan.core, executor: x, turnID: req.TurnID}
	}
	return plan
}

func (x *ADKExecutor) withheldTools(offered []tool.Tool) []string {
	var withheld []string
	for _, t := range x.tools {
		name := t.Name()
		_, plugin := x.pluginTools[name]
		_, external := x.externalServers[name]
		_, channel := x.channelTools[name]
		if x.isOperationTool(name) || plugin || external || channel || name == auratools.KBSearchToolName {
			continue
		}
		if slices.ContainsFunc(offered, func(o tool.Tool) bool { return o.Name() == name }) {
			continue
		}
		withheld = append(withheld, name)
	}
	slices.Sort(withheld)
	return withheld
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
	names := make([]string, 0, len(p.core))
	for _, t := range p.core {
		names = append(names, t.Name())
	}
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
	plan     *operationPlan
	core     []tool.Tool
	executor *ADKExecutor
	turnID   string
}

func (s *operationToolset) Name() string { return "aura_operations" }

func (s *operationToolset) Tools(ctx agent.ReadonlyContext) ([]tool.Tool, error) {
	names, err := s.plan.loadedNow(ctx.SessionID())
	if err != nil {
		s.executor.logger.WarnContext(ctx, "operation toolset unavailable", "component", "turn", "turn_id", s.turnID, "error", fmt.Errorf("read the session history: %w", err))
		return nil, nil
	}
	loaded := make([]tool.Tool, 0, len(names))
	for _, name := range names {
		if slices.ContainsFunc(s.core, func(t tool.Tool) bool { return t.Name() == name }) {
			continue
		}
		if found, ok := s.executor.toolIndex[name]; ok {
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
