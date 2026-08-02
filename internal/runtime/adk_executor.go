package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"log/slog"
	"strings"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/store"

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
}

// ToolBroker is the policy gate every tool call must pass before execution.
// approval.Engine is the canonical implementation.
type ToolBroker interface {
	Evaluate(ctx context.Context, request *approval.ToolRequest) (approval.PolicyDecision, error)
}

// NewADKExecutor builds an ADK-backed turn executor. modelName is resolved
// through the ADK model registry (registered by the model package at
// startup); broker is the tool policy gate; tools are the declared tool set
// (empty until the built-in tools engine lands — every declared tool is
// still gated by the broker before ADK executes it).
func NewADKExecutor(appName, modelName string, sessions SessionPort, events EventStore, broker ToolBroker, tools []tool.Tool, logger *slog.Logger) (*ADKExecutor, error) {
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
	return &ADKExecutor{
		appName:   appName,
		modelName: modelName,
		sessions:  sessions,
		events:    events,
		broker:    broker,
		tools:     tools,
		logger:    logger,
	}, nil
}

// Execute runs one turn through the ADK runner. The returned events carry
// full ADK fidelity (invocation, branch, author, actions, usage); the engine
// stamps sequence and persists them.
func (x *ADKExecutor) Execute(ctx context.Context, req *TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
	return func(yield func(store.RuntimeEvent, error) bool) {
		sessionService, err := NewADKSessionService(x.sessions, x.events)
		if err != nil {
			yield(store.RuntimeEvent{}, err)
			return
		}
		adkRunner, err := x.buildRunner(ctx, sessionService)
		if err != nil {
			yield(store.RuntimeEvent{}, err)
			return
		}

		content, err := contentFromParts(req)
		if err != nil {
			yield(store.RuntimeEvent{}, err)
			return
		}

		usage := &usageTracker{maxTokens: req.Budget.MaxTokens}
		for ev, err := range adkRunner.Run(ctx, req.PrincipalID, req.SessionID, content, agent.RunConfig{}) {
			if err != nil {
				if usage.exceeded {
					yield(store.RuntimeEvent{}, codedError(ErrorCodeBudgetExhausted, "turn budget exhausted", nil))
					return
				}
				yield(store.RuntimeEvent{}, err)
				return
			}
			if exceeded := usage.add(ev); exceeded {
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

// toolGate evaluates one tool invocation through the broker. A deny or an
// evaluation error blocks the call with a stable code.
func (x *ADKExecutor) toolGate(ctx context.Context, toolName string, args map[string]any) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return codedError(ErrorCodeRuntimeInternal, "failed to marshal tool arguments", err)
	}
	decision, err := x.broker.Evaluate(ctx, &approval.ToolRequest{
		ToolName:  toolName,
		Arguments: raw,
		Trust:     approval.TrustDerivedUntrusted,
	})
	if err != nil {
		return codedError(ErrorCodePolicyDenied, "tool evaluation failed closed", err)
	}
	if decision.Outcome != "allow" {
		return codedError(ErrorCodePolicyDenied, "tool call denied by policy", nil)
	}
	return nil
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
	if err := x.toolGate(actx, t.Name(), args); err != nil {
		return nil, err
	}
	return args, nil
}
