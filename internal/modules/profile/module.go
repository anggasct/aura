package profile

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/metric"
	adkmodel "google.golang.org/adk/v2/model"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/telemetry"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

type Deps struct {
	Config       Config
	DB           *sql.DB
	Sessions     SessionStore
	Events       EventStore
	TaintReader  SessionTaintAtReader
	Models       ModelSource
	ModelConfig  model.Config
	ModelRoutes  model.Routes
	Scanner      SecretScanner
	Owner        *principal.Owner
	Meters       metric.MeterProvider
	CheckTimeout time.Duration
	Logger       *slog.Logger
}

var _ operation.Provider = (*Module)(nil)

type Module struct {
	service      *Service
	operations   *Operations
	extractor    *Extractor
	health       *profileHealth
	checkTimeout time.Duration
	maxFacts     int
	maxTokens    int
	stated       []string
}

func New(ctx context.Context, deps *Deps) (*Module, error) {
	if deps == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "profile dependencies must not be nil")
	}
	if deps.DB == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "profile database must not be nil")
	}
	if deps.Scanner == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "profile scanner must not be nil")
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	store, err := NewSQLiteStore(deps.DB)
	if err != nil {
		return nil, err
	}
	var actions ActionSink
	if deps.Sessions != nil && deps.Events != nil {
		sink, err := NewActionSink(deps.Sessions, deps.Events)
		if err != nil {
			return nil, err
		}
		actions = sink
	}
	mod := &Module{
		health:       &profileHealth{},
		checkTimeout: deps.CheckTimeout,
		maxFacts:     deps.Config.MaxContextFacts,
		maxTokens:    deps.Config.MaxContextTokens,
		stated:       StatedOwnerKeys(deps.Owner),
	}
	recorder, err := telemetry.NewProfileRecorder(deps.Meters, telemetryCallbacks(store, func() int64 {
		if mod.extractor == nil {
			return 0
		}
		return int64(mod.extractor.QueueDepth())
	}))
	if err != nil {
		return nil, err
	}
	record := recorderObserver(recorder)
	observer := func(ctx context.Context, observation *Observation) {
		record(ctx, observation)
		mod.health.observe(observation)
	}
	mod.service, err = NewService(store, ServiceOptions{
		MinConfidence: deps.Config.CandidateMinConfidence,
		Scanner:       deps.Scanner,
		Actions:       actions,
		Observer:      observer,
	})
	if err != nil {
		return nil, err
	}
	mod.operations, err = NewOperations(&OperationDeps{Service: mod.service})
	if err != nil {
		return nil, err
	}
	llm, producer, err := resolveProfilingLLM(ctx, deps.ModelConfig, deps.ModelRoutes, deps.Models)
	if err != nil {
		logger.WarnContext(ctx, "profile extraction unavailable", "component", "profile", "error", err)
	}
	if llm == nil {
		mod.health.unavailable.Store(true)
		return mod, nil
	}
	taintReader := deps.TaintReader
	if taintReader == nil {
		taintReader = turnstore.NewTaintReader(deps.DB)
	}
	mod.extractor, err = NewExtractor(
		mod.service,
		NewModelCaller(llm, producer, extractMaxOutputTokens),
		deps.Scanner,
		"profiling",
		deps.Config.PromptVersion,
		extractMaxOutputTokens,
		deps.Config.ExtractionQueueCapacity,
		logger,
		WithObserver(observer),
		WithTaintReader(taintReader),
	)
	if err != nil {
		return nil, err
	}
	return mod, nil
}

func resolveProfilingLLM(ctx context.Context, models model.Config, routes model.Routes, source ModelSource) (adkmodel.LLM, ModelProducer, error) {
	if source == nil {
		return nil, ModelProducer{}, nil
	}
	producer, err := ResolveProducer(models, routes)
	if err != nil {
		return nil, ModelProducer{}, err
	}
	llm, err := source.LLM(ctx, ProfilingRole(models))
	if err != nil {
		return nil, ModelProducer{}, err
	}
	return llm, producer, nil
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

func (m *Module) Extractor() *Extractor {
	if m == nil {
		return nil
	}
	return m.extractor
}

func (m *Module) Run(ctx context.Context) {
	if m != nil && m.extractor != nil {
		m.extractor.Run(ctx)
	}
}

func recorderObserver(recorder *telemetry.ProfileRecorder) Observer {
	return func(ctx context.Context, observation *Observation) {
		if observation == nil {
			return
		}
		recorder.Record(ctx, &telemetry.ProfileObservation{
			Kind:       observation.Kind,
			Result:     observation.Result,
			QueueAge:   observation.QueueAge,
			Lag:        observation.Lag,
			Accepted:   observation.Accepted,
			Screened:   observation.Screened,
			Skipped:    observation.Skipped,
			Facts:      observation.Facts,
			Tokens:     observation.Tokens,
			Overridden: observation.Overridden,
		})
	}
}

func telemetryCallbacks(store *SQLiteStore, queueDepth func() int64) telemetry.ProfileCallbacks {
	callbacks := telemetry.ProfileCallbacks{QueueDepth: queueDepth}
	if store != nil {
		callbacks.FactCounts = func(ctx context.Context) []telemetry.ProfileFactCount {
			rows, err := store.FactCounts(ctx)
			if err != nil {
				return nil
			}
			counts := make([]telemetry.ProfileFactCount, 0, len(rows))
			for i := range rows {
				counts = append(counts, telemetry.ProfileFactCount{
					Status:   rows[i].Status,
					Category: rows[i].Category,
					Count:    rows[i].Count,
				})
			}
			return counts
		}
	}
	return callbacks
}
