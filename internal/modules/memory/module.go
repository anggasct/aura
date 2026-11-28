package memory

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

type Deps struct {
	Config      Config
	DB          *sql.DB
	Sessions    SessionReader
	TaintMarks  TaintMarksReader
	Scopes      RecallScopes
	Secrets     Secrets
	Models      ModelSource
	ModelConfig *model.Config
	ModelRoutes map[string]model.ModelRoute
	Observer    Observer
	Logger      *slog.Logger
}

var (
	_ operation.BehaviorProvider = (*Module)(nil)
	_ operation.Provider         = (*Module)(nil)
)

type Module struct {
	cfg          Config
	service      *Service
	provider     *Provider
	summaryModel *SummaryModel
	summarizer   Summarizer
	operations   *Operations
	logger       *slog.Logger
}

func New(ctx context.Context, deps *Deps) (*Module, error) {
	if deps == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "memory deps must not be nil")
	}
	if deps.DB == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "memory database must not be nil")
	}
	if deps.Sessions == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "memory sessions must not be nil")
	}
	if deps.Scopes == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "memory recall scopes must not be nil")
	}
	if deps.Secrets == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "memory secrets must not be nil")
	}
	store, err := NewSQLiteStore(deps.DB, deps.Sessions)
	if err != nil {
		return nil, err
	}
	service, err := NewService(store, deps.Secrets, &ServiceOptions{
		MaxDocuments:         deps.Config.MaxDocuments,
		TokenBudget:          deps.Config.RecallTokenBudget,
		SummaryPromptVersion: deps.Config.SummaryPromptVersion,
		SummaryTTL:           time.Duration(deps.Config.SummaryTTL),
		RangeThreshold:       deps.Config.RangeThreshold,
		RangeLiveWindow:      deps.Config.RangeLiveWindow,
		Observer:             deps.Observer,
	})
	if err != nil {
		return nil, err
	}
	taintMarks := deps.TaintMarks
	if taintMarks == nil {
		taintMarks = turnstore.NewTaintReader(deps.DB)
	}
	summaryModel, summarizer := BuildSummary(ctx, deps.ModelConfig, deps.ModelRoutes, deps.Models, deps.Logger, deps.Observer)
	provider, err := NewProvider(service, &ProviderConfig{Scopes: deps.Scopes, TaintMarks: taintMarks})
	if err != nil {
		return nil, err
	}
	operations, err := NewOperations(&OperationDeps{Service: service, Scopes: deps.Scopes, TaintMarks: taintMarks, Logger: deps.Logger})
	if err != nil {
		return nil, err
	}
	return &Module{
		cfg:          deps.Config,
		service:      service,
		provider:     provider,
		summaryModel: summaryModel,
		summarizer:   summarizer,
		operations:   operations,
		logger:       deps.Logger,
	}, nil
}

func (m *Module) Behaviors() []operation.Behavior {
	if m == nil {
		return nil
	}
	return []operation.Behavior{{ID: "memory", Group: "memory", Summary: "past conversations are recalled into your context when they match"}}
}

func (m *Module) Bindings() []operation.Binding {
	if m == nil || m.operations == nil {
		return nil
	}
	return m.operations.Bindings()
}

func (m *Module) Service() *Service {
	if m == nil {
		return nil
	}
	return m.service
}

func (m *Module) TurnContext(ctx context.Context, req *turn.TurnRequest) ([]turn.ContextBlock, error) {
	if m == nil || m.provider == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "memory module must not be nil")
	}
	return m.provider.TurnContext(ctx, req)
}

func (m *Module) Required(req *turn.TurnRequest) bool {
	if m == nil || m.provider == nil {
		return false
	}
	return m.provider.Required(req)
}

func (m *Module) PurgeLoop() func(context.Context) {
	if m == nil || m.service == nil || m.cfg.PurgeInterval <= 0 || m.cfg.PurgeLimit <= 0 {
		return nil
	}
	interval := time.Duration(m.cfg.PurgeInterval)
	limit := m.cfg.PurgeLimit
	service := m.service
	return func(ctx context.Context) {
		service.RunPurgeLoop(ctx, interval, limit)
	}
}
