package app

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/modules/schedule"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/store"
)

const (
	personaEvalComponent            = "persona_eval"
	codePersonaEvalRegressed        = "persona_eval_regressed"
	codePersonaEvalLookupFailed     = "persona_eval_lookup_failed"
	remediationPersonaEvalRegressed = "re-run `aura persona eval <id> --behavior` after reviewing the persona; set evals.behavior.enforce_on_regression to stop serving non-owners on regression"
)

type personaEvalHealth struct {
	registry *auraagent.Registry
	results  store.PersonaEvalStore
}

func personaEvalCheck(cfg *config.Config, registry *auraagent.Registry, db *sql.DB) health.RegisteredCheck {
	return health.RegisteredCheck{
		ID:          personaEvalComponent,
		Checker:     &personaEvalHealth{registry: registry, results: store.NewPersonaEvalStore(db)},
		Timeout:     time.Duration(cfg.Health.CheckTimeout),
		Remediation: remediationPersonaEvalRegressed,
	}
}

func (h *personaEvalHealth) Check(ctx context.Context) []health.Finding {
	var findings []health.Finding
	definitions := h.registry.Definitions()
	for i := range definitions {
		definition := &definitions[i]
		if definition.EvalBehavior == nil {
			continue
		}
		regressed, err := h.regressed(ctx, definition)
		if err != nil {
			findings = append(findings, health.Finding{
				Component: personaEvalComponent, Scope: definition.ID, Code: codePersonaEvalLookupFailed,
				Status: health.StatusUnknown, Detail: "persona evaluation results could not be read",
			})
			continue
		}
		if regressed {
			findings = append(findings, health.Finding{
				Component: personaEvalComponent, Scope: definition.ID, Code: codePersonaEvalRegressed,
				Status: health.StatusDegraded, Severity: health.SeverityWarning,
				Detail:      "persona " + definition.ID + " failed its latest behavior evaluation after passing it for the same version",
				Remediation: remediationPersonaEvalRegressed,
			})
		}
	}
	return findings
}

func (h *personaEvalHealth) regressed(ctx context.Context, definition *auraagent.Definition) (bool, error) {
	digest := definition.EvalDigest()
	latest, found, err := h.results.Latest(ctx, definition.ID, digest, store.EvalLayerBehavior)
	if err != nil || !found || latest.Passed {
		return false, err
	}
	return h.results.EverPassed(ctx, definition.ID, digest, store.EvalLayerBehavior)
}

type evalClock struct {
	now   func() time.Time
	after func(time.Duration) <-chan time.Time
}

var wallEvalClock = evalClock{now: time.Now, after: time.After}

func runPersonaEvalSchedules(ctx context.Context, configPath string, db *sql.DB, registry *auraagent.Registry, models behaviorModels, logger *slog.Logger, clock evalClock) {
	var wg sync.WaitGroup
	definitions := registry.Definitions()
	for i := range definitions {
		definition := &definitions[i]
		if definition.EvalBehavior == nil || definition.EvalBehavior.Schedule == "" {
			continue
		}
		wg.Go(func() {
			runPersonaEvalSchedule(ctx, configPath, db, definition, models, logger, clock)
		})
	}
	wg.Wait()
}

func runPersonaEvalSchedule(ctx context.Context, configPath string, db *sql.DB, definition *auraagent.Definition, models behaviorModels, logger *slog.Logger, clock evalClock) {
	cronSchedule, err := schedule.ParseExpression(definition.EvalBehavior.Schedule)
	if err != nil {
		logger.WarnContext(ctx, "persona eval schedule invalid", "component", personaEvalComponent, "agent_id", definition.ID, "error", err)
		return
	}
	for {
		next, err := schedule.NextFire(&cronSchedule, time.UTC, clock.now())
		if err != nil {
			logger.WarnContext(ctx, "persona eval schedule has no next fire", "component", personaEvalComponent, "agent_id", definition.ID, "error", err)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-clock.after(next.Sub(clock.now())):
		}
		recorded, report, err := runPersonaBehaviorEval(ctx, db, configPath, definition, models)
		if err != nil {
			logger.WarnContext(ctx, "persona eval scheduled run failed", "component", personaEvalComponent, "agent_id", definition.ID, "error", err)
			continue
		}
		logger.InfoContext(ctx, "persona eval scheduled", "component", personaEvalComponent, "agent_id", definition.ID,
			"passed", recorded.Passed, "score", report.Score, "min_score", report.MinScore, "cases_failed", recorded.CasesFailed)
	}
}
