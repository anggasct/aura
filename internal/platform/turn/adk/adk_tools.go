package runtimeadk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

type BuiltinToolDefinition struct {
	Name                 string
	Version              string
	Server               string
	Plugin               string
	Description          string
	Schema               json.RawMessage
	RequiredCapabilities []string
	OptIn                bool
}

type BuiltinToolRequest struct {
	RequestID       string
	TurnID          string
	SessionID       string
	PrincipalID     string
	PrincipalClass  principal.Class
	AgentID         string
	ToolName        string
	ToolVersion     string
	Arguments       json.RawMessage
	Capabilities    []string
	Trust           string
	Deadline        time.Time
	IdempotencyKey  string
	EventInvocation string
	EventBranch     string
	EventAuthor     string
	Knowledge       []string
	Tainted         bool
}

type BuiltinToolExecutor interface {
	Definitions() []BuiltinToolDefinition
	Execute(context.Context, *BuiltinToolRequest) (json.RawMessage, error)
}

type turnIDContextKey struct{}

func withTurnID(ctx context.Context, turnID string) context.Context {
	return context.WithValue(ctx, turnIDContextKey{}, turnID)
}

func turnIDFromContext(ctx context.Context) string {
	turnID, _ := ctx.Value(turnIDContextKey{}).(string)
	return turnID
}

type turnAccessContextKey struct{}

type turnAccess struct {
	principalID string
	class       principal.Class
	agentID     string
	knowledge   []string
	tainted     bool
}

func withTurnAccess(ctx context.Context, req *turn.TurnRequest, agentID string) context.Context {
	access := turnAccess{principalID: req.PrincipalID, class: req.PrincipalClass, agentID: agentID, knowledge: slices.Clone(req.Knowledge), tainted: req.Tainted()}
	return context.WithValue(ctx, turnAccessContextKey{}, access)
}

func turnAccessFromContext(ctx context.Context) turnAccess {
	access, _ := ctx.Value(turnAccessContextKey{}).(turnAccess)
	return access
}

func buildBuiltinTools(definitions []BuiltinToolDefinition) ([]tool.Tool, error) {
	if len(definitions) == 0 {
		return nil, invalidArgument("builtin tool definitions must not be empty")
	}
	tools := make([]tool.Tool, 0, len(definitions))
	seen := make(map[string]struct{}, len(definitions))
	for i := range definitions {
		definition := &definitions[i]
		if definition.Name == "" || definition.Version == "" {
			return nil, invalidArgument("builtin tool name and version must not be empty")
		}
		if _, ok := seen[definition.Name]; ok {
			return nil, invalidArgument(fmt.Sprintf("duplicate builtin tool %q", definition.Name))
		}
		seen[definition.Name] = struct{}{}
		var schema jsonschema.Schema
		if err := json.Unmarshal(definition.Schema, &schema); err != nil {
			return nil, fmt.Errorf("decode schema for builtin tool %q: %w", definition.Name, err)
		}
		name := definition.Name
		builtin, err := functiontool.New[map[string]any, map[string]any](
			functiontool.Config{
				Name:        name,
				Description: definition.Description,
				InputSchema: &schema,
			},
			func(agent.Context, map[string]any) (map[string]any, error) {
				return nil, errors.New("builtin tool invocation must pass through the broker")
			},
		)
		if err != nil {
			return nil, fmt.Errorf("build builtin tool %q: %w", name, err)
		}
		tools = append(tools, builtin)
	}
	return tools, nil
}

func cloneBuiltinDefinitions(definitions []BuiltinToolDefinition) []BuiltinToolDefinition {
	cloned := make([]BuiltinToolDefinition, len(definitions))
	for i := range definitions {
		cloned[i] = definitions[i]
		cloned[i].Schema = slices.Clone(definitions[i].Schema)
		cloned[i].RequiredCapabilities = slices.Clone(definitions[i].RequiredCapabilities)
	}
	return cloned
}
