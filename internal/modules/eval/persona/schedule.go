package personaeval

import (
	"context"
	"log/slog"
	"sync"
	"time"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/fault"
)

type ParseCron func(expr string) (func(loc *time.Location, now time.Time) (time.Time, error), error)

type Clock struct {
	Now   func() time.Time
	After func(time.Duration) <-chan time.Time
}

type ScheduleDeps struct {
	ConfigPath string
	Registry   *auraagent.Registry
	Service    *Service
	ParseCron  ParseCron
	Logger     *slog.Logger
	Clock      Clock
}

type ScheduleRunner struct {
	configPath string
	registry   *auraagent.Registry
	service    *Service
	parseCron  ParseCron
	logger     *slog.Logger
	clock      Clock
}

func NewScheduleRunner(deps *ScheduleDeps) (*ScheduleRunner, error) {
	if deps == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "persona eval schedule dependencies must not be nil")
	}
	if deps.Registry == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "persona eval schedule registry must not be nil")
	}
	if deps.Service == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "persona eval schedule service must not be nil")
	}
	if deps.ParseCron == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "persona eval schedule cron parser must not be nil")
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	clock := deps.Clock
	if clock.Now == nil {
		clock.Now = time.Now
	}
	if clock.After == nil {
		clock.After = time.After
	}
	return &ScheduleRunner{
		configPath: deps.ConfigPath,
		registry:   deps.Registry,
		service:    deps.Service,
		parseCron:  deps.ParseCron,
		logger:     logger,
		clock:      clock,
	}, nil
}

func (r *ScheduleRunner) Run(ctx context.Context) {
	if r == nil || r.registry == nil {
		return
	}
	var wg sync.WaitGroup
	definitions := r.registry.Definitions()
	for i := range definitions {
		definition := &definitions[i]
		if definition.EvalBehavior == nil || definition.EvalBehavior.Schedule == "" {
			continue
		}
		wg.Go(func() {
			r.RunPersona(ctx, definition)
		})
	}
	wg.Wait()
}

func (r *ScheduleRunner) RunPersona(ctx context.Context, definition *auraagent.Definition) {
	if r == nil || definition == nil || definition.EvalBehavior == nil {
		return
	}
	nextFire, err := r.parseCron(definition.EvalBehavior.Schedule)
	if err != nil {
		r.logger.WarnContext(ctx, "persona eval schedule invalid", "component", Component, "agent_id", definition.ID, "error", err)
		return
	}
	for {
		next, err := nextFire(time.UTC, r.clock.Now())
		if err != nil {
			r.logger.WarnContext(ctx, "persona eval schedule has no next fire", "component", Component, "agent_id", definition.ID, "error", err)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-r.clock.After(next.Sub(r.clock.Now())):
		}
		recorded, report, err := r.service.RunBehavior(ctx, r.configPath, definition)
		if err != nil {
			r.logger.WarnContext(ctx, "persona eval scheduled run failed", "component", Component, "agent_id", definition.ID, "error", err)
			continue
		}
		r.logger.InfoContext(ctx, "persona eval scheduled", "component", Component, "agent_id", definition.ID,
			"passed", recorded.Passed, "score", report.Score, "min_score", report.MinScore, "cases_failed", recorded.CasesFailed)
	}
}
