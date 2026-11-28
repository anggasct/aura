package app

import (
	"context"
	"encoding/json"
	"iter"
	"log/slog"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/effect"
	auramodel "github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeadk "github.com/anggasct/aura/internal/platform/turn/adk"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

type skillTaintProvider struct {
	block turn.ContextBlock
}

func (p skillTaintProvider) Required(*turn.TurnRequest) bool { return false }

func (p skillTaintProvider) TurnContext(_ context.Context, _ *turn.TurnRequest) ([]turn.ContextBlock, error) {
	return []turn.ContextBlock{p.block}, nil
}

type skillProbeLLM struct {
	called bool
}

func (m *skillProbeLLM) Name() string { return "skill-probe-model" }

func (m *skillProbeLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if !m.called {
			m.called = true
			yield(&adkmodel.LLMResponse{
				Content: &genai.Content{
					Role: "model",
					Parts: []*genai.Part{{
						FunctionCall: &genai.FunctionCall{
							ID:   "call-skill-probe",
							Name: "skill_probe",
							Args: map[string]any{},
						},
					}},
				},
				TurnComplete: true,
			}, nil)
			return
		}
		yield(&adkmodel.LLMResponse{
			Content:      &genai.Content{Role: "model", Parts: []*genai.Part{{Text: "done"}}},
			TurnComplete: true,
		}, nil)
	}
}

func TestSkillContextBlockTaintsExecutorTurns(t *testing.T) {
	db := sqlitetest.OpenMigrated(t, turnstore.Schema(), effect.Schema())
	sessions := turnstore.NewSessionService(db)
	now := time.Now().UTC()
	if err := sessions.Create(t.Context(), &turn.Session{
		ID: "session-skill-1", OwnerID: principal.OwnerID, AgentID: "main", CreatedAt: now, UpdatedAt: now, Metadata: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	journal, err := effect.NewJournal(db, effect.Options{Turns: turnstore.NewTurnStore(db)})
	if err != nil {
		t.Fatalf("effect.NewJournal: %v", err)
	}
	effects, err := effect.NewExecutor(journal)
	if err != nil {
		t.Fatalf("effect.NewExecutor: %v", err)
	}
	broker, err := toolbroker.New(&toolbroker.Options{Effects: effects})
	if err != nil {
		t.Fatalf("toolbroker.New: %v", err)
	}
	var seenTainted bool
	var seenMark turn.TaintMark
	probeDef := tools.Definition{
		Name:      "skill_probe",
		Version:   tools.Version,
		Schema:    json.RawMessage(`{"type":"object"}`),
		Validator: func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil },
	}
	probeRule := toolbroker.DefaultRule(&probeDef)
	if err := broker.RegisterTool(&probeDef, func(_ context.Context, req *toolbroker.ToolRequest, _ approval.Constraints) (toolbroker.ToolResult, error) {
		seenTainted = req.Tainted
		seenMark = req.TaintMark
		return toolbroker.ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
	}, &probeRule); err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}
	builtin, err := runtimeadk.NewBrokerExecutor(broker, capabilitytest.Host(t))
	if err != nil {
		t.Fatalf("NewBrokerExecutor: %v", err)
	}
	provider := skillTaintProvider{
		block: turn.ContextBlock{
			Content: "SKILL BODY",
			Taint:   &turn.TaintMark{Source: turn.TaintSourceSkill, Detail: "hostile-skill"},
		},
	}
	executor, err := runtimeadk.NewADKExecutor(
		"aura",
		"skill-probe-model",
		auramodel.Set{"skill-probe-model": &skillProbeLLM{}},
		sessions,
		builtin,
		nil,
		slog.New(slog.DiscardHandler),
		runtimeadk.WithBuiltinToolExecutor(builtin),
		runtimeadk.WithContextProvider(provider),
	)
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	var marks []turn.TaintMark
	for ev, err := range executor.Execute(t.Context(), &turn.TurnRequest{
		TurnID:         "turn-skill-1",
		SessionID:      "session-skill-1",
		PrincipalID:    principal.OwnerID,
		PrincipalClass: principal.ClassOwner,
		Origin:         turn.OriginTerminal,
		Parts:          []runtimeingress.InputPart{{Text: "run the skill"}},
	}) {
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if ev.Kind == turn.EventKindSessionTainted {
			mark, ok := turn.DecodeTaintMark(ev.Payload)
			if !ok {
				t.Fatalf("invalid taint mark payload: %s", ev.Payload)
			}
			marks = append(marks, mark)
		}
	}
	if len(marks) != 1 || marks[0].Source != turn.TaintSourceSkill || marks[0].Detail != "hostile-skill" {
		t.Fatalf("taint marks = %+v, want one skill mark for hostile-skill", marks)
	}
	if !seenTainted || seenMark.Source != turn.TaintSourceSkill || seenMark.Detail != "hostile-skill" {
		t.Fatalf("tool saw tainted=%v mark=%+v, want tainted=true source=%s detail=hostile-skill", seenTainted, seenMark, turn.TaintSourceSkill)
	}
}
