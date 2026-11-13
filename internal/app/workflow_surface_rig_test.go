package app

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	kitapproval "github.com/anggasct/aura/internal/channels/kit/approval"
	"github.com/anggasct/aura/internal/config"
	toolsbuiltin "github.com/anggasct/aura/internal/modules/builtin"
	"github.com/anggasct/aura/internal/modules/workflow"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/telemetry"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/store/storetest"
)

type workflowCard struct {
	prompt    kitapproval.Prompt
	requester kitapproval.Requester
}

type workflowOwnerCards struct {
	mu     sync.Mutex
	answer func(prompt *kitapproval.Prompt) bool
	cards  []workflowCard
}

func (o *workflowOwnerCards) DecideForOwner(_ context.Context, prompt *kitapproval.Prompt, requester *kitapproval.Requester) (bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.cards = append(o.cards, workflowCard{prompt: *prompt, requester: *requester})
	if o.answer == nil {
		return true, nil
	}
	return o.answer(prompt), nil
}

type observationLog struct {
	mu   sync.Mutex
	seen []toolbroker.Observation
}

func (l *observationLog) record(observation toolbroker.Observation) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen = append(l.seen, observation)
}

type surfaceInput struct {
	cfg      *config.Config
	result   *config.LoadResult
	db       *sql.DB
	registry *auraagent.Registry
	owner    *workflowOwnerCards
	logger   *slog.Logger
}

type surfaceRig struct {
	runner       *workflowToolRunner
	executor     *toolsbuiltin.Executor
	db           *sql.DB
	workspace    string
	registry     *auraagent.Registry
	owner        *workflowOwnerCards
	route        *kitapproval.OwnerRoute
	deciderCalls atomic.Int64
	observed     *observationLog
	spans        *tracetest.InMemoryExporter
	metrics      *sdkmetric.ManualReader
}

func buildSurface(t *testing.T, in *surfaceInput) *surfaceRig {
	t.Helper()
	result := in.result
	if result == nil {
		result = loadResultWithAll(t, in.cfg)
	}
	registry := in.registry
	if registry == nil {
		built, err := buildAgentRegistry(result.Config)
		if err != nil {
			t.Fatalf("buildAgentRegistry: %v", err)
		}
		registry = built
	}
	_, artifactRoot, _, err := storagePaths(result.Config)
	if err != nil {
		t.Fatalf("storagePaths: %v", err)
	}
	logger := in.logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	rig := &surfaceRig{db: in.db, registry: registry, owner: in.owner, observed: &observationLog{}, spans: tracetest.NewInMemoryExporter(), metrics: sdkmetric.NewManualReader()}
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(rig.spans))
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(rig.metrics))
	t.Cleanup(func() {
		_ = tracerProvider.Shutdown(context.Background())
		_ = meterProvider.Shutdown(context.Background())
	})
	recorder, err := telemetry.NewToolRecorder(tracerProvider, meterProvider)
	if err != nil {
		t.Fatalf("NewToolRecorder: %v", err)
	}
	record := toolObserver(recorder)
	var route *kitapproval.OwnerRoute
	var targets []approvalChannel
	if in.owner != nil {
		route, err = kitapproval.NewOwnerRoute(&workflowOwnerApprover{owners: in.owner, now: time.Now})
		if err != nil {
			t.Fatalf("NewOwnerRoute: %v", err)
		}
		targets = append(targets, route)
	}
	inner := channelApprovalDecider(targets)
	wiring := &toolWiring{
		observer: func(ctx context.Context, observation toolbroker.Observation) {
			rig.observed.record(observation)
			record(ctx, observation)
		},
		decider: func(ctx context.Context, prompt *toolbroker.ApprovalPrompt) (bool, error) {
			rig.deciderCalls.Add(1)
			if inner == nil {
				return false, nil
			}
			return inner(ctx, prompt)
		},
		access:    newAccessPolicy(registry, in.db, nil),
		reachable: channelApprovalReachable(targets),
	}
	executor, err := newToolExecutor(&toolExecutorInput{
		result: result, db: in.db, artifactRoot: artifactRoot, logger: slog.New(slog.DiscardHandler), wiring: wiring,
		events: turn.NewEventBus(slog.New(slog.DiscardHandler)),
	})
	if err != nil {
		t.Fatalf("newToolExecutor: %v", err)
	}
	built, err := newWorkflowToolRunner(&workflowToolRunnerInput{
		Executor: executor, Sessions: store.NewSessionService(in.db), Registry: registry, Runs: workflow.NewStore(in.db),
		Route: route, Logger: logger,
	})
	if err != nil {
		t.Fatalf("newWorkflowToolRunner: %v", err)
	}
	runner, ok := built.(*workflowToolRunner)
	if !ok {
		t.Fatalf("newWorkflowToolRunner returned %T, want *workflowToolRunner", built)
	}
	rig.runner, rig.executor, rig.route, rig.workspace = runner, executor, route, result.Config.Tools.Workspace
	return rig
}

type surfaceRigOptions struct {
	configure func(*config.Config)
	owner     *workflowOwnerCards
	logger    *slog.Logger
}

func newSurfaceRig(t *testing.T, opts *surfaceRigOptions) *surfaceRig {
	t.Helper()
	dir := t.TempDir()
	workspace := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	cfg := config.Default()
	cfg.Tools.Workspace = workspace
	cfg.Storage.Path = dir
	if opts.configure != nil {
		opts.configure(&cfg)
	}
	storetest.Seed(t, filepath.Join(dir, "aura.db"))
	db, err := store.OpenDB(t.Context(), filepath.Join(dir, "aura.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return buildSurface(t, &surfaceInput{cfg: &cfg, db: db, owner: opts.owner, logger: opts.logger})
}
