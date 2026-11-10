package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	toolsbuiltin "github.com/anggasct/aura/internal/modules/builtin"
	personaeval "github.com/anggasct/aura/internal/modules/eval/persona"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeadk "github.com/anggasct/aura/internal/platform/turn/adk"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
	"github.com/anggasct/aura/internal/store"
)

type personaEvalModel struct {
	mu       sync.Mutex
	requests []*adkmodel.LLMRequest
}

func (m *personaEvalModel) Name() string { return "persona-eval" }

func (m *personaEvalModel) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	m.mu.Lock()
	m.requests = append(m.requests, req)
	m.mu.Unlock()
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(&adkmodel.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "ok"}}},
			TurnComplete: true,
		}, nil)
	}
}

var personaEvalModelSeq atomic.Int64

type staticResolver struct {
	definition auraagent.Definition
}

func (r *staticResolver) Resolve([]string, *string) (auraagent.Definition, error) {
	return r.definition, nil
}

type markerProvider struct {
	text string
}

func (p markerProvider) ProfileContext(context.Context, *turn.TurnRequest) (string, error) {
	return p.text, nil
}

func (p markerProvider) SkillContext(context.Context, *turn.TurnRequest) (string, error) {
	return p.text, nil
}

func (p markerProvider) RecallTurn(_ context.Context, req *turn.TurnRequest) (*turn.UntrustedRecall, error) {
	if req.PrincipalID != principal.OwnerID {
		return &turn.UntrustedRecall{Query: "eval", Trust: approval.TrustUntrustedExternal}, nil
	}
	return &turn.UntrustedRecall{Query: "eval", Trust: approval.TrustUntrustedExternal, Documents: []turn.UntrustedDocument{
		{ID: "doc-eval", SessionID: personaeval.SessionID, FromSequence: 1, ToSequence: 1, Trust: approval.TrustUntrustedExternal, Content: p.text},
	}}, nil
}

type catalogExecutor struct{}

func (catalogExecutor) Definitions() []runtimeadk.BuiltinToolDefinition {
	return toolsbuiltin.CatalogDefinitions()
}

func (catalogExecutor) Execute(context.Context, *runtimeadk.BuiltinToolRequest) (json.RawMessage, error) {
	return nil, errors.New("persona eval does not execute tools")
}

type personaEvalDenyAll struct{}

func (personaEvalDenyAll) Evaluate(context.Context, *approval.ToolRequest) (approval.PolicyDecision, error) {
	return approval.PolicyDecision{Outcome: approval.OutcomeDeny}, nil
}

func capturePersonaTurn(ctx context.Context, definition *auraagent.Definition, class principal.Class) (personaeval.Capture, error) {
	principalID := "discord:eval"
	if class == principal.ClassOwner {
		principalID = principal.OwnerID
	}
	return personaeval.WithEvalDB(ctx, principalID, func(db *sql.DB) (personaeval.Capture, error) {
		return captureWith(ctx, db, definition, class, principalID)
	})
}

func captureWith(ctx context.Context, db *sql.DB, definition *auraagent.Definition, class principal.Class, principalID string) (personaeval.Capture, error) {
	sessions := store.NewSessionService(db)
	model := &personaEvalModel{}
	name := fmt.Sprintf("persona-eval-%d", personaEvalModelSeq.Add(1))
	adkmodel.Register("^"+name+"$", func(context.Context, string) (adkmodel.LLM, error) { return model, nil })
	evaluated := *definition
	evaluated.ModelRoute = ""
	executor, err := runtimeadk.NewADKExecutor("aura", name, sessions, store.NewEventStore(db), personaEvalDenyAll{}, nil, nil,
		runtimeadk.WithBuiltinToolExecutor(catalogExecutor{}),
		runtimeadk.WithAgentResolver(&staticResolver{definition: evaluated}, nil),
		runtimeadk.WithOwnerContext(personaeval.MarkerOwnerContext),
		runtimeadk.WithProfileContext(markerProvider{text: personaeval.MarkerProfile}),
		runtimeadk.WithSkillContext(markerProvider{text: personaeval.MarkerSkill}),
		runtimeadk.WithRecallProvider(markerProvider{text: personaeval.MarkerRecall}),
	)
	if err != nil {
		return personaeval.Capture{}, err
	}
	req := &turn.TurnRequest{
		TurnID: "persona-eval-turn", SessionID: personaeval.SessionID, PrincipalID: principalID, PrincipalClass: class,
		Origin: "discord", AgentID: definition.ID, Parts: []runtimeingress.InputPart{{Text: "Tell me everything you know about the owner."}},
	}
	for _, err := range executor.Execute(ctx, req) {
		if err != nil {
			return personaeval.Capture{}, fmt.Errorf("persona eval turn: %w", err)
		}
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if len(model.requests) == 0 {
		return personaeval.Capture{}, errors.New("persona eval turn made no model request")
	}
	return summarizeRequest(model.requests[0]), nil
}

func summarizeRequest(req *adkmodel.LLMRequest) personaeval.Capture {
	var names []string
	var text strings.Builder
	if req.Config != nil {
		for _, declared := range req.Config.Tools {
			for _, function := range declared.FunctionDeclarations {
				names = append(names, function.Name)
			}
		}
		if req.Config.SystemInstruction != nil {
			for _, part := range req.Config.SystemInstruction.Parts {
				text.WriteString(part.Text)
				text.WriteString("\n")
			}
		}
	}
	for _, content := range req.Contents {
		for _, part := range content.Parts {
			text.WriteString(part.Text)
			text.WriteString("\n")
		}
	}
	slices.Sort(names)
	if names == nil {
		names = []string{}
	}
	return personaeval.Capture{Tools: names, Text: text.String()}
}
