package personaeval

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"slices"
	"time"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/tools"
)

type JudgeFactory func(route string) Judge

type ServiceDeps struct {
	Store    Store
	Capturer Capturer
	Converse Converser
	JudgeFor JudgeFactory
	Catalog  []tools.Definition
	Now      func() time.Time
}

type Service struct {
	store    Store
	capturer Capturer
	converse Converser
	judgeFor JudgeFactory
	catalog  []tools.Definition
	now      func() time.Time
}

func NewService(deps *ServiceDeps) (*Service, error) {
	if deps == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "persona eval service dependencies must not be nil")
	}
	if deps.Store == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "persona eval service store must not be nil")
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		store:    deps.Store,
		capturer: deps.Capturer,
		converse: deps.Converse,
		judgeFor: deps.JudgeFor,
		catalog:  slices.Clone(deps.Catalog),
		now:      now,
	}, nil
}

func EvalDir(configPath string, definition *auraagent.Definition) string {
	base := filepath.Dir(configPath)
	if definition.EvalDir != "" {
		if filepath.IsAbs(definition.EvalDir) {
			return definition.EvalDir
		}
		return filepath.Join(base, definition.EvalDir)
	}
	return filepath.Join(base, "evals", definition.ID)
}

func (s *Service) RunPolicy(ctx context.Context, configPath string, definition *auraagent.Definition) (Result, error) {
	if s == nil || s.store == nil {
		return Result{}, fault.New(ErrorCodeInvalidArgument, "persona eval service is not initialized")
	}
	if definition == nil {
		return Result{}, fault.New(ErrorCodeInvalidArgument, "persona definition must not be nil")
	}
	cases, err := LoadCases(EvalDir(configPath, definition))
	if err != nil {
		return Result{}, err
	}
	report, err := RunWithCatalog(ctx, definition, cases, s.capturer, s.catalog)
	if err != nil {
		return Result{}, err
	}
	return s.store.Record(ctx, &Result{
		AgentID:     definition.ID,
		Digest:      definition.EvalDigest(),
		Layer:       LayerPolicy,
		Passed:      report.Passed(),
		CasesTotal:  report.Total,
		CasesFailed: report.Failed,
		FailedCases: report.FailedCases,
		CreatedAt:   s.now().UTC(),
	})
}

func (s *Service) RunBehavior(ctx context.Context, configPath string, definition *auraagent.Definition) (Result, BehaviorReport, error) {
	if s == nil || s.store == nil {
		return Result{}, BehaviorReport{}, fault.New(ErrorCodeInvalidArgument, "persona eval service is not initialized")
	}
	if definition == nil {
		return Result{}, BehaviorReport{}, fault.New(ErrorCodeInvalidArgument, "persona definition must not be nil")
	}
	if definition.EvalBehavior == nil {
		return Result{}, BehaviorReport{}, invalid("persona %q does not enable behavior evaluation", definition.ID)
	}
	if s.converse == nil || s.judgeFor == nil {
		return Result{}, BehaviorReport{}, invalid("behavior evaluation needs a converser and a judge")
	}
	cases, err := LoadCases(EvalDir(configPath, definition))
	if err != nil {
		return Result{}, BehaviorReport{}, err
	}
	report, err := RunBehavior(ctx, definition, cases, s.converse, s.judgeFor(definition.EvalBehavior.JudgeRoute))
	if err != nil {
		return Result{}, BehaviorReport{}, err
	}
	score := report.Score
	recorded, err := s.store.Record(ctx, &Result{
		AgentID:     definition.ID,
		Digest:      definition.EvalDigest(),
		Layer:       LayerBehavior,
		Passed:      report.Passed(),
		Score:       &score,
		CasesTotal:  len(report.Cases),
		CasesFailed: len(report.FailedCases),
		FailedCases: report.FailedCases,
		CreatedAt:   s.now().UTC(),
	})
	return recorded, report, err
}

func (s *Service) List(ctx context.Context, agentID string, limit int) ([]Result, error) {
	if s == nil || s.store == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "persona eval service is not initialized")
	}
	return s.store.List(ctx, agentID, limit)
}

type ModuleDeps struct {
	DB           *sql.DB
	Registry     *auraagent.Registry
	ConfigPath   string
	Capturer     Capturer
	Converse     Converser
	JudgeFor     JudgeFactory
	Catalog      []tools.Definition
	ParseCron    ParseCron
	CheckTimeout time.Duration
	Logger       *slog.Logger
	Clock        Clock
}

type Module struct {
	store        *SQLiteStore
	service      *Service
	gate         *Gate
	health       *HealthChecker
	scheduler    *ScheduleRunner
	checkTimeout time.Duration
}

func New(deps *ModuleDeps) (*Module, error) {
	if deps == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "persona eval module dependencies must not be nil")
	}
	if deps.DB == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "persona eval module database must not be nil")
	}
	if deps.Registry == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "persona eval module registry must not be nil")
	}
	store, err := NewSQLiteStore(deps.DB)
	if err != nil {
		return nil, err
	}
	service, err := NewService(&ServiceDeps{
		Store:    store,
		Capturer: deps.Capturer,
		Converse: deps.Converse,
		JudgeFor: deps.JudgeFor,
		Catalog:  deps.Catalog,
		Now:      deps.Clock.Now,
	})
	if err != nil {
		return nil, err
	}
	gate, err := NewGate(deps.Registry, store)
	if err != nil {
		return nil, err
	}
	checker, err := NewHealthChecker(deps.Registry, store)
	if err != nil {
		return nil, err
	}
	var scheduler *ScheduleRunner
	if deps.ParseCron != nil {
		scheduler, err = NewScheduleRunner(&ScheduleDeps{
			ConfigPath: deps.ConfigPath,
			Registry:   deps.Registry,
			Service:    service,
			ParseCron:  deps.ParseCron,
			Logger:     deps.Logger,
			Clock:      deps.Clock,
		})
		if err != nil {
			return nil, err
		}
	}
	return &Module{
		store:        store,
		service:      service,
		gate:         gate,
		health:       checker,
		scheduler:    scheduler,
		checkTimeout: deps.CheckTimeout,
	}, nil
}

func (m *Module) Store() Store {
	if m == nil {
		return nil
	}
	return m.store
}

func (m *Module) Service() *Service {
	if m == nil {
		return nil
	}
	return m.service
}

func (m *Module) Gate() *Gate {
	if m == nil {
		return nil
	}
	return m.gate
}

func (m *Module) HealthChecks() []health.RegisteredCheck {
	if m == nil || m.health == nil {
		return nil
	}
	return []health.RegisteredCheck{{
		ID:          Component,
		Checker:     m.health,
		Timeout:     m.checkTimeout,
		Remediation: RemediationRegressed,
	}}
}

func (m *Module) RunSchedules(ctx context.Context) {
	if m == nil || m.scheduler == nil {
		return
	}
	m.scheduler.Run(ctx)
}
