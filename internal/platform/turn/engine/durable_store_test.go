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

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/durable/sqlite"
	runtimesessions "github.com/anggasct/aura/internal/platform/turn/sessions"
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

func newDurableTestRuntime(t *testing.T, cfg Config, executor TurnExecutor) (*Engine, *hookedSessionStore, *sql.DB) {
	t.Helper()
	engine, db, _ := newTestRuntime(t, cfg, executor)
	sessions := newHookedSessionStore(t, db)
	engine.sessionStore = sessions
	return engine, sessions, db
}

func newDurableLiveTestRuntime(t *testing.T, cfg Config, executor TurnExecutor) (*Engine, *hookedSessionStore, *sql.DB) {
	t.Helper()
	engine, db, _ := newTestRuntime(t, cfg, executor)
	sessions := newHookedSessionStore(t, db)
	backend := durable.NewFake()
	engine.sessionStore = sessions
	engine.durableRuntime = backend
	backend.RegisterHandler("turn", engine.serveTurn)
	return engine, sessions, db
}

const (
	hostRigPoll    = 10 * time.Millisecond
	hostRigTimeout = 20 * time.Second
)

type durableHostRig struct {
	t        *testing.T
	cfg      Config
	db       *sql.DB
	sessions *hookedSessionStore
	runtime  *sqlite.Runtime
	engine   *Engine
	host     *runningHost
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

func newDurableHostRig(t *testing.T, cfg Config, executor TurnExecutor) *durableHostRig {
	t.Helper()
	db := openTestDatabase(t)
	rig := &durableHostRig{t: t, cfg: cfg, db: db, sessions: newHookedSessionStore(t, db)}
	rig.build(executor)
	return rig
}

func (r *durableHostRig) build(executor TurnExecutor) {
	r.t.Helper()
	rt, err := sqlite.New(&sqlite.Options{DB: r.db, Logger: slog.New(slog.DiscardHandler), PollInterval: hostRigPoll, StopGrace: 5 * time.Second})
	if err != nil {
		r.t.Fatalf("sqlite.New: %v", err)
	}
	cfg := r.cfg
	cfg.Durable = &DurableConfig{Sessions: r.sessions, Runtime: rt}
	engine, err := NewEngine(&cfg, store.NewEventStore(r.db), store.NewDedupeStore(r.db), executor, slog.New(slog.DiscardHandler))
	if err != nil {
		r.t.Fatalf("NewEngine: %v", err)
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
