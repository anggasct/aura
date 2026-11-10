package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/config"
	personaeval "github.com/anggasct/aura/internal/modules/eval/persona"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeadk "github.com/anggasct/aura/internal/platform/turn/adk"
	runtimeengine "github.com/anggasct/aura/internal/platform/turn/engine"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
	"github.com/anggasct/aura/internal/store"
)

const judgeMaxOutputTokens = 512

type behaviorModels struct {
	defaultModel string
	route        func(string) (string, error)
}

func registerBehaviorModels(ctx context.Context, cfg *config.Config, db *sql.DB, logger *slog.Logger) (behaviorModels, error) {
	if err := model.RegisterAdaptersWithRoutes(ctx, logger, cfg.Models, cfg.ModelRoutes, &storeCircuitCheckpointAdapter{store: store.NewCircuitCheckpointStore(db)}, nil); err != nil {
		return behaviorModels{}, err
	}
	return routedBehaviorModels(cfg)
}

func routedBehaviorModels(cfg *config.Config) (behaviorModels, error) {
	defaultModel := cfg.Models.Definitions["primary"].Model
	route, err := model.RouteModelName(cfg, defaultModel)
	if err != nil {
		return behaviorModels{}, err
	}
	return behaviorModels{defaultModel: defaultModel, route: route}, nil
}

func (m behaviorModels) converse(ctx context.Context, definition *auraagent.Definition, class principal.Class, turns []string) ([]personaeval.Exchange, error) {
	return personaeval.WithEvalDB(ctx, "discord:eval", func(db *sql.DB) ([]personaeval.Exchange, error) {
		events := store.NewEventStore(db)
		executor, err := runtimeadk.NewADKExecutor("aura", m.defaultModel, store.NewSessionService(db), events, personaEvalDenyAll{}, nil, nil,
			runtimeadk.WithBuiltinToolExecutor(catalogExecutor{}),
			runtimeadk.WithAgentResolver(&staticResolver{definition: *definition}, m.route),
			runtimeadk.WithOwnerContext(personaeval.MarkerOwnerContext),
			runtimeadk.WithProfileContext(markerProvider{text: personaeval.MarkerProfile}),
			runtimeadk.WithSkillContext(markerProvider{text: personaeval.MarkerSkill}),
			runtimeadk.WithRecallProvider(markerProvider{text: personaeval.MarkerRecall}),
		)
		if err != nil {
			return nil, err
		}
		transcript := make([]personaeval.Exchange, 0, len(turns))
		for i, text := range turns {
			req := &turn.TurnRequest{
				TurnID: fmt.Sprintf("persona-eval-behavior-%d", i+1), SessionID: personaeval.SessionID, PrincipalID: "discord:eval", PrincipalClass: class,
				Origin: "discord", AgentID: definition.ID, Parts: []runtimeingress.InputPart{{Text: text}},
			}
			var produced []store.RuntimeEvent
			for event, err := range executor.Execute(ctx, req) {
				if err != nil {
					return nil, fmt.Errorf("persona turn %d: %w", i+1, err)
				}
				if err := persistEvalEvent(ctx, events, req, &event); err != nil {
					return nil, err
				}
				produced = append(produced, event)
			}
			transcript = append(transcript, personaeval.Exchange{User: text, Assistant: strings.Join(runtimeengine.DeliveryTexts(produced), "\n")})
		}
		return transcript, nil
	})
}

func persistEvalEvent(ctx context.Context, events store.EventStore, req *turn.TurnRequest, event *store.RuntimeEvent) error {
	if event.ID == "" {
		event.ID = runtimeengine.NewTurnID()
	}
	event.SessionID, event.TurnID = req.SessionID, req.TurnID
	if event.Author == "" {
		event.Author = req.PrincipalID
	}
	if event.SchemaVersion == 0 {
		event.SchemaVersion = 1
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	_, err := events.AppendSequenced(ctx, req.SessionID, event)
	return err
}

func (m behaviorModels) judge(route string) personaeval.Judge {
	return func(ctx context.Context, prompt string) (string, error) {
		name, err := m.route(route)
		if err != nil {
			return "", err
		}
		llm, err := adkmodel.NewLLM(ctx, name)
		if err != nil {
			return "", fmt.Errorf("resolve judge model %q: %w", name, err)
		}
		var temperature float32
		var collected strings.Builder
		for response, err := range llm.GenerateContent(ctx, &adkmodel.LLMRequest{
			Contents: []*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{{Text: prompt}}}},
			Config:   &genai.GenerateContentConfig{Temperature: &temperature, MaxOutputTokens: judgeMaxOutputTokens},
		}, false) {
			if err != nil {
				return "", err
			}
			if response == nil || response.Content == nil {
				continue
			}
			for _, part := range response.Content.Parts {
				if part == nil {
					continue
				}
				if collected.Len()+len(part.Text) > personaeval.JudgeMaxOutputBytes {
					return "", errors.New("judge output exceeds the bound")
				}
				collected.WriteString(part.Text)
			}
		}
		return collected.String(), nil
	}
}

func runPersonaBehaviorEval(ctx context.Context, db *sql.DB, configPath string, definition *auraagent.Definition, models behaviorModels) (store.PersonaEvalResult, personaeval.BehaviorReport, error) {
	if definition.EvalBehavior == nil {
		return store.PersonaEvalResult{}, personaeval.BehaviorReport{}, fmt.Errorf("%s: persona %q does not enable behavior evaluation", personaeval.ErrorCodeInvalid, definition.ID)
	}
	cases, err := personaeval.LoadCases(personaEvalDir(configPath, definition))
	if err != nil {
		return store.PersonaEvalResult{}, personaeval.BehaviorReport{}, err
	}
	report, err := personaeval.RunBehavior(ctx, definition, cases, models.converse, models.judge(definition.EvalBehavior.JudgeRoute))
	if err != nil {
		return store.PersonaEvalResult{}, personaeval.BehaviorReport{}, err
	}
	score := report.Score
	recorded, err := store.NewPersonaEvalStore(db).Record(ctx, &store.PersonaEvalResult{
		AgentID: definition.ID, Digest: definition.EvalDigest(), Layer: store.EvalLayerBehavior, Passed: report.Passed(), Score: &score,
		CasesTotal: len(report.Cases), CasesFailed: len(report.FailedCases), FailedCases: report.FailedCases, CreatedAt: time.Now().UTC(),
	})
	return recorded, report, err
}

func behaviorLines(report *personaeval.BehaviorReport) []string {
	lines := make([]string, 0, 1+len(report.Cases))
	lines = append(lines, fmt.Sprintf("behavior score: %.2f (min %.2f)", report.Score, report.MinScore))
	for _, scored := range report.Cases {
		lines = append(lines, fmt.Sprintf("case %s: %.2f — %s", scored.Name, scored.Score, scored.Reason))
	}
	return lines
}
