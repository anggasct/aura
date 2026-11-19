package runtimeengine

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/durable/durabletest"
	"github.com/anggasct/aura/internal/platform/durable/sqlite"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimesessions "github.com/anggasct/aura/internal/platform/turn/sessions"
	"github.com/anggasct/aura/internal/platform/turn/turntest"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/store/storetest"
)

type hookedSessionStore struct {
	*sqlite.SessionStore

	mu            sync.Mutex
	afterAdmit    func(turnID string)
	aroundRelease func(sessionID, turnID string, release func() (runtimesessions.ReleaseResult, error)) (runtimesessions.ReleaseResult, error)
	aroundStatus  func(sessionID string, status func() (runtimesessions.StatusResult, error)) (runtimesessions.StatusResult, error)
	aroundActive  func(list func() ([]runtimesessions.QueuedTurn, error)) ([]runtimesessions.QueuedTurn, error)
}

func newHookedSessionStore(t *testing.T, db *sql.DB) *hookedSessionStore {
	t.Helper()
	sessions, err := sqlite.NewSessionStore(&sqlite.SessionStoreOptions{DB: db})
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}
	return &hookedSessionStore{SessionStore: sessions}
}

func (s *hookedSessionStore) hooks(apply func(s *hookedSessionStore)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	apply(s)
}

func (s *hookedSessionStore) Admit(ctx context.Context, req *runtimesessions.AdmitRequest) (runtimesessions.AdmitResult, error) {
	result, err := s.SessionStore.Admit(ctx, req)
	s.mu.Lock()
	after := s.afterAdmit
	s.mu.Unlock()
	if err == nil && after != nil {
		after(req.Turn.TurnID)
	}
	return result, err
}

func (s *hookedSessionStore) Release(ctx context.Context, sessionID, turnID string) (runtimesessions.ReleaseResult, error) {
	s.mu.Lock()
	around := s.aroundRelease
	s.mu.Unlock()
	release := func() (runtimesessions.ReleaseResult, error) { return s.SessionStore.Release(ctx, sessionID, turnID) }
	if around == nil {
		return release()
	}
	return around(sessionID, turnID, release)
}

func (s *hookedSessionStore) Status(ctx context.Context, sessionID string) (runtimesessions.StatusResult, error) {
	s.mu.Lock()
	around := s.aroundStatus
	s.mu.Unlock()
	status := func() (runtimesessions.StatusResult, error) { return s.SessionStore.Status(ctx, sessionID) }
	if around == nil {
		return status()
	}
	return around(sessionID, status)
}

func (s *hookedSessionStore) ActiveTurns(ctx context.Context) ([]runtimesessions.QueuedTurn, error) {
	s.mu.Lock()
	around := s.aroundActive
	s.mu.Unlock()
	list := func() ([]runtimesessions.QueuedTurn, error) { return s.SessionStore.ActiveTurns(ctx) }
	if around == nil {
		return list()
	}
	return around(list)
}

func newDurableLiveTestRuntime(t *testing.T, deps Deps, executor TurnExecutor) (*Engine, *hookedSessionStore, *sql.DB) {
	t.Helper()
	engine, db, _ := newTestRuntime(t, deps, executor)
	sessions := newHookedSessionStore(t, db)
	backend := durabletest.NewFake()
	engine.sessionStore = sessions
	engine.durableRuntime = backend
	backend.RegisterHandler("turn", engine.serveTurn)
	t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })
	return engine, sessions, db
}

const (
	hostRigPoll    = 10 * time.Millisecond
	hostRigTimeout = 20 * time.Second
)

type durableHostRig struct {
	t        *testing.T
	cfg      Deps
	db       *sql.DB
	sessions *hookedSessionStore
	runtime  *sqlite.Runtime
	engine   *Engine
	host     *runningHost
	hostTTL  time.Duration
}

type runningHost struct {
	cancel context.CancelFunc
	result chan error
	once   sync.Once
}

func openTestDatabase(t *testing.T) *sql.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "aura.db")
	storetest.Seed(t, dsn)
	db, err := store.OpenDB(t.Context(), dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func newDurableHostRig(t *testing.T, deps Deps, executor TurnExecutor) *durableHostRig {
	t.Helper()
	db := openTestDatabase(t)
	rig := &durableHostRig{t: t, cfg: deps, db: db, sessions: newHookedSessionStore(t, db)}
	rig.build(executor)
	return rig
}

const shortHostTTL = time.Second

func newShortLeaseHostRig(t *testing.T, deps Deps, executor TurnExecutor) *durableHostRig {
	t.Helper()
	db := openTestDatabase(t)
	rig := &durableHostRig{t: t, cfg: deps, db: db, sessions: newHookedSessionStore(t, db), hostTTL: shortHostTTL}
	rig.build(executor)
	return rig
}

func (r *durableHostRig) build(executor TurnExecutor) {
	r.t.Helper()
	rt, err := sqlite.New(&sqlite.Options{DB: r.db, Logger: slog.New(slog.DiscardHandler), PollInterval: hostRigPoll, StopGrace: 5 * time.Second, HostTTL: r.hostTTL})
	if err != nil {
		r.t.Fatalf("sqlite.New: %v", err)
	}
	deps := r.cfg
	deps.Durable = &DurableConfig{Sessions: r.sessions, Runtime: rt}
	deps.Events, deps.Dedupe, deps.Executor = store.NewEventStore(r.db), store.NewDedupeStore(r.db), executor
	deps.Logger = slog.New(slog.DiscardHandler)
	engine, err := New(&deps)
	if err != nil {
		r.t.Fatalf("New: %v", err)
	}
	engine.statusPoll = hostRigPoll
	r.runtime = rt
	r.engine = engine
	r.host = nil
}

func (r *durableHostRig) startHost() {
	r.t.Helper()
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.t.Context()))
	h := &runningHost{cancel: cancel, result: make(chan error, 1)}
	rt := r.runtime
	go func() { h.result <- rt.Run(ctx) }()
	r.host = h
	engine := r.engine
	r.t.Cleanup(func() {
		_ = h.stop()
		shutdownCtx, cancelShutdown := context.WithTimeout(context.WithoutCancel(r.t.Context()), time.Second)
		defer cancelShutdown()
		_ = engine.Shutdown(shutdownCtx)
	})
}

func (h *runningHost) stop() error {
	var err error
	h.once.Do(func() {
		h.cancel()
		select {
		case err = <-h.result:
		case <-time.After(hostRigTimeout):
			err = errors.New("durable host did not stop")
		}
	})
	return err
}

func (r *durableHostRig) stopHost() {
	r.t.Helper()
	if r.host == nil {
		return
	}
	if err := r.host.stop(); err != nil {
		r.t.Fatalf("stop durable host: %v", err)
	}
}

func (r *durableHostRig) restart(executor TurnExecutor) {
	r.t.Helper()
	r.stopHost()
	r.build(executor)
	r.startHost()
}

func TestNewEngineRefusesADurableConfigWithoutAStoreOrARuntime(t *testing.T) {
	db := openTestDatabase(t)
	sessions := newHookedSessionStore(t, db)
	configs := map[string]*DurableConfig{
		"no runtime":       {Sessions: sessions},
		"no session store": {Runtime: durabletest.NewFake()},
	}
	for name, durableCfg := range configs {
		t.Run(name, func(t *testing.T) {
			_, err := New(&Deps{Events: store.NewEventStore(db), Dedupe: store.NewDedupeStore(db), Executor: turntest.NewFakeExecutor(nil), Config: Default(), Durable: durableCfg})
			if !fault.HasCode(err, turn.ErrorCodeInvalidArgument) {
				t.Fatalf("New err = %v, want invalid_argument", err)
			}
		})
	}
}

func TestDurableAdmissionRefusesAUsedTurnIDAsInvalidArgument(t *testing.T) {
	executor := turntest.NewFakeExecutor([]turntest.FakeStep{textStep("hello")})
	rig := newDurableHostRig(t, Deps{Config: testLimits(2, 16)}, executor)
	mustCreateSession(t, rig.db, "session-a")
	rig.startHost()

	if _, err := collect(t, rig.engine, sampleRequest("session-a", "turn-1")); err != nil {
		t.Fatalf("first run of turn-1: %v", err)
	}
	_, err := collect(t, rig.engine, sampleRequest("session-a", "turn-1"))
	code, ok := fault.CodeOf[turn.ErrorCode](err)
	if !ok || code != turn.ErrorCodeInvalidArgument {
		t.Fatalf("run under a used turn id = %v, want %s", err, turn.ErrorCodeInvalidArgument)
	}
	if code.Retryable() {
		t.Fatalf("%s is retryable, want a refusal that is not", code)
	}
	if got := executor.StartCount(); got != 1 {
		t.Fatalf("executor starts = %d, want 1: the refused turn must not start a run", got)
	}
}

func TestNewRefusesZeroLimits(t *testing.T) {
	cases := map[string]func(*Config){
		"max active turns":    func(c *Config) { c.MaxActiveTurns = 0 },
		"max pending turns":   func(c *Config) { c.MaxPendingTurns = 0 },
		"turn timeout":        func(c *Config) { c.TurnTimeout = 0 },
		"shutdown timeout":    func(c *Config) { c.ShutdownTimeout = 0 },
		"run retention floor": func(c *Config) { c.RunRetention = configkit.Duration(MinRunRetention - time.Second) },
	}
	db := openTestDatabase(t)
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			mutate(&cfg)
			_, err := New(&Deps{Events: store.NewEventStore(db), Dedupe: store.NewDedupeStore(db), Executor: turntest.NewFakeExecutor(nil), Config: cfg})
			if !fault.HasCode(err, turn.ErrorCodeInvalidArgument) {
				t.Fatalf("New err = %v, want invalid_argument", err)
			}
		})
	}
}
