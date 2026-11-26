package app

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	appconfig "github.com/anggasct/aura/internal/app/config"
	personaeval "github.com/anggasct/aura/internal/modules/eval/persona"
	"github.com/anggasct/aura/internal/modules/schedule"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
)

func loadPersona(cfg *appconfig.Config, id string) (auraagent.Definition, error) {
	registry, err := buildAgentRegistry(cfg)
	if err != nil {
		return auraagent.Definition{}, err
	}
	definition, ok := registry.Lookup(id)
	if !ok {
		return auraagent.Definition{}, fmt.Errorf("agent_not_found: no persona has id %q", id)
	}
	return definition, nil
}

func parsePersonaEvalCron(expr string) (func(loc *time.Location, now time.Time) (time.Time, error), error) {
	parsed, err := schedule.ParseExpression(expr)
	if err != nil {
		return nil, err
	}
	return func(loc *time.Location, now time.Time) (time.Time, error) {
		return schedule.NextFire(&parsed, loc, now)
	}, nil
}

func personaEvalOpener(gf *globalFlags) personaeval.Opener {
	return func(cmd *cobra.Command, req personaeval.CommandRequest, fn func(ctx context.Context, env personaeval.CommandEnv) error) error {
		result, err := appconfig.Load(gf.configPath)
		if err != nil {
			return err
		}
		definition, err := loadPersona(result.Config, req.PersonaID)
		if err != nil {
			return err
		}
		var parts []Part
		if req.NeedBehavior && definition.EvalBehavior != nil {
			parts = []Part{PartModels}
		}
		return withApp(cmd, &commandInput{result: &result, parts: parts}, func(ctx context.Context, app *App) error {
			evalStore, err := personaeval.NewSQLiteStore(app.db)
			if err != nil {
				return err
			}
			deps := &personaeval.ServiceDeps{
				Store:    evalStore,
				Capturer: capturePersonaTurn,
			}
			if req.NeedBehavior && definition.EvalBehavior != nil {
				models, err := routedBehaviorModels(result.Config, app.models)
				if err != nil {
					return err
				}
				deps.Converse = models.converse
				deps.JudgeFor = models.judge
			}
			svc, err := personaeval.NewService(deps)
			if err != nil {
				return err
			}
			return fn(ctx, personaeval.CommandEnv{
				ConfigPath: result.Path,
				Definition: definition,
				Service:    svc,
			})
		})
	}
}
