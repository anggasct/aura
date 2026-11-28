package schedule

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation"
)

const DefaultPassInterval = 5 * time.Minute

type ModuleDeps struct {
	Config       Config
	DB           *sql.DB
	Turns        TurnRunner
	Notify       Notifier
	Starter      Starter
	PassInterval time.Duration
	Logger       *slog.Logger
	Observer     Observer

	WorkflowChecker WorkflowChecker
	Owner           OwnerDestination
}

type Module struct {
	store        *SQLiteStore
	service      *Service
	runner       *Runner
	starter      Starter
	passInterval time.Duration
	logger       *slog.Logger
	operations   *Operations
}

func New(deps *ModuleDeps) (*Module, error) {
	if deps == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "schedule module deps must not be nil")
	}
	if deps.DB == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "schedule database must not be nil")
	}
	if deps.Turns == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "schedule turn runner must not be nil")
	}
	if deps.Notify == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "schedule notifier must not be nil")
	}
	if deps.Starter == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "schedule starter must not be nil")
	}
	store, err := NewSQLiteStore(deps.DB)
	if err != nil {
		return nil, err
	}
	service, err := NewService(store)
	if err != nil {
		return nil, err
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	runner, err := NewRunner(&RunnerDeps{
		Jobs:      store,
		Turns:     deps.Turns,
		Notify:    deps.Notify,
		Starter:   deps.Starter,
		Logger:    logger,
		Retention: time.Duration(deps.Config.OccurrenceRetention),
		Observer:  deps.Observer,
	})
	if err != nil {
		return nil, err
	}
	operations, err := NewOperations(&OperationDeps{
		Config: deps.Config, Store: store, Service: service, Starter: deps.Starter,
		WorkflowChecker: deps.WorkflowChecker, Owner: deps.Owner,
	})
	if err != nil {
		return nil, err
	}
	passInterval := deps.PassInterval
	if passInterval <= 0 {
		passInterval = DefaultPassInterval
	}
	return &Module{
		store:        store,
		service:      service,
		runner:       runner,
		starter:      deps.Starter,
		passInterval: passInterval,
		logger:       logger,
		operations:   operations,
	}, nil
}

func (m *Module) Bindings() []operation.Binding {
	return m.operations.Bindings()
}

func (m *Module) Store() *SQLiteStore {
	return m.store
}

func (m *Module) Service() *Service {
	return m.service
}

func (m *Module) Runner() *Runner {
	return m.runner
}

func (m *Module) Handle(ctx context.Context, inv durable.Invocation) error {
	return m.runner.Handle(ctx, inv)
}

func (m *Module) Resume(ctx context.Context) (int, error) {
	return StartRuns(ctx, &RunsRequest{
		Starter: m.starter,
		Store:   m.store,
		Now:     time.Now().UTC(),
		Reuse:   durable.ReuseEnded,
	})
}

func (m *Module) RunPass(ctx context.Context) {
	ticker := time.NewTicker(m.passInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := StartRuns(ctx, &RunsRequest{
				Starter: m.starter,
				Store:   m.store,
				Now:     time.Now().UTC(),
			}); err != nil && ctx.Err() == nil {
				m.logger.WarnContext(ctx, "schedule pass failed", "component", "schedule", "error", err)
			}
		}
	}
}
