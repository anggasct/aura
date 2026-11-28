package schedule

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/durable/durabletest"
	durablesqlite "github.com/anggasct/aura/internal/platform/durable/sqlite"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	"github.com/anggasct/aura/internal/platform/turn/store"
)

type stubTurnRunner struct{}

func (stubTurnRunner) RunTurn(context.Context, *TurnRequest) (TurnResult, error) {
	return TurnResult{Succeeded: true}, nil
}

type stubNotifier struct{}

func (stubNotifier) Notify(context.Context, *NotifyRequest) error {
	return nil
}

type moduleRecordingStarter struct {
	mu       sync.Mutex
	requests []durable.StartRequest
}

func (s *moduleRecordingStarter) Start(_ context.Context, req durable.StartRequest) (durable.RunRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, req)
	return durable.RunRef{Key: req.Key}, nil
}

func (s *moduleRecordingStarter) snapshot() []durable.StartRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]durable.StartRequest, len(s.requests))
	copy(out, s.requests)
	return out
}

func TestScheduleModuleNewValidatesDeps(t *testing.T) {
	db := sqlitetest.OpenMigrated(t, turnstore.Schema(), durablesqlite.Schema(), operation.Schema(), Schema())
	rt := durabletest.NewFake()
	cases := []struct {
		name string
		deps *ModuleDeps
	}{
		{"nil deps", nil},
		{"nil db", &ModuleDeps{Turns: stubTurnRunner{}, Notify: stubNotifier{}, Starter: rt}},
		{"nil turns", &ModuleDeps{DB: db, Notify: stubNotifier{}, Starter: rt}},
		{"nil notify", &ModuleDeps{DB: db, Turns: stubTurnRunner{}, Starter: rt}},
		{"nil starter", &ModuleDeps{DB: db, Turns: stubTurnRunner{}, Notify: stubNotifier{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.deps); !fault.HasCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("New() err = %v, want invalid_argument", err)
			}
		})
	}
}

func TestScheduleModuleResumeUsesReuseEnded(t *testing.T) {
	db := sqlitetest.OpenMigrated(t, turnstore.Schema(), durablesqlite.Schema(), operation.Schema(), Schema())
	starter := &moduleRecordingStarter{}
	mod, err := New(&ModuleDeps{
		Config:  Default(),
		DB:      db,
		Turns:   stubTurnRunner{},
		Notify:  stubNotifier{},
		Starter: starter,
		Logger:  slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	if mod.Store() == nil || mod.Service() == nil || mod.Runner() == nil {
		t.Fatal("module getters returned nil")
	}
	if err := mod.Store().InsertJob(t.Context(), testScheduledJob("job-resume")); err != nil {
		t.Fatalf("InsertJob(): %v", err)
	}
	fire := time.Date(2026, 9, 12, 21, 0, 0, 0, time.UTC)
	if err := mod.Store().RecordFire(t.Context(), &Occurrence{
		ID: "occ-resume", JobID: "job-resume", JobVersion: 1, ScheduledForUTC: fire,
		State: OccurrenceFired, CreatedAt: fire, UpdatedAt: fire,
	}); err != nil {
		t.Fatalf("RecordFire(): %v", err)
	}

	count, err := mod.Resume(t.Context())
	if err != nil {
		t.Fatalf("Resume(): %v", err)
	}
	if count != 1 {
		t.Fatalf("Resume() count = %d, want 1", count)
	}
	reqs := starter.snapshot()
	if len(reqs) != 1 || reqs[0].Reuse != durable.ReuseEnded {
		t.Fatalf("Resume() requests = %+v, want ReuseEnded", reqs)
	}
}

func TestScheduleModuleRunPassUsesReuseNeverAndStopsOnCancel(t *testing.T) {
	db := sqlitetest.OpenMigrated(t, turnstore.Schema(), durablesqlite.Schema(), operation.Schema(), Schema())
	starter := &moduleRecordingStarter{}
	mod, err := New(&ModuleDeps{
		Config:       Default(),
		DB:           db,
		Turns:        stubTurnRunner{},
		Notify:       stubNotifier{},
		Starter:      starter,
		PassInterval: 5 * time.Millisecond,
		Logger:       slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	if err := mod.Store().InsertJob(t.Context(), testScheduledJob("job-pass")); err != nil {
		t.Fatalf("InsertJob(): %v", err)
	}
	fire := time.Date(2026, 9, 12, 21, 0, 0, 0, time.UTC)
	if err := mod.Store().RecordFire(t.Context(), &Occurrence{
		ID: "occ-pass", JobID: "job-pass", JobVersion: 1, ScheduledForUTC: fire,
		State: OccurrenceFired, CreatedAt: fire, UpdatedAt: fire,
	}); err != nil {
		t.Fatalf("RecordFire(): %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		mod.RunPass(ctx)
	}()

	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for len(starter.snapshot()) == 0 {
		select {
		case <-tick.C:
		case <-deadline.C:
			cancel()
			t.Fatal("timed out waiting for RunPass tick")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunPass did not stop after cancel")
	}

	reqs := starter.snapshot()
	if reqs[0].Reuse != durable.ReuseNever {
		t.Fatalf("RunPass() reuse = %v, want ReuseNever", reqs[0].Reuse)
	}
}
