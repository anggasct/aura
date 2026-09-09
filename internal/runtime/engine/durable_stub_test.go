package runtimeengine

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/runtime"
	runtimesessions "github.com/anggasct/aura/internal/runtime/sessions"
	"github.com/anggasct/aura/internal/store"
)

type stubSessionStore struct {
	mu     sync.Mutex
	states map[string]*runtimesessions.State
	events store.EventStore
	dedupe store.DedupeStore
}

func newStubSessionStore(events store.EventStore, dedupe store.DedupeStore) *stubSessionStore {
	return &stubSessionStore{states: map[string]*runtimesessions.State{}, events: events, dedupe: dedupe}
}

func (s *stubSessionStore) stateLocked(sessionID string) *runtimesessions.State {
	state, ok := s.states[sessionID]
	if !ok {
		state = runtimesessions.NewState()
		s.states[sessionID] = state
	}
	return state
}

func (s *stubSessionStore) Admit(ctx context.Context, req *runtimesessions.AdmitRequest) (runtimesessions.AdmitResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.stateLocked(req.Turn.SessionID)
	if original, err := runtimesessions.CheckReplay(state, req.Turn.IdempotencyKey); err != nil {
		return runtimesessions.AdmitResult{}, err
	} else if original != "" {
		return runtimesessions.AdmitResult{Replayed: true, OriginalTurnID: original}, nil
	}
	accepted := &store.RuntimeEvent{
		ID:            req.AcceptedEventID,
		SessionID:     req.Turn.SessionID,
		TurnID:        req.Turn.TurnID,
		Author:        req.Turn.PrincipalID,
		Kind:          runtime.EventKindTurnAccepted,
		SchemaVersion: 1,
		Payload:       acceptedPayloadFor(&req.Turn),
		CreatedAt:     time.Now().UTC(),
	}
	if req.Turn.IdempotencyKey == "" {
		sequence, err := s.events.AppendSequenced(ctx, req.Turn.SessionID, accepted)
		if err != nil {
			return runtimesessions.AdmitResult{}, err
		}
		accepted.Sequence = sequence
	} else {
		original, created, err := s.dedupe.Accept(ctx, req.Turn.Origin, req.Turn.IdempotencyKey, time.Now().UTC().Add(24*time.Hour), accepted)
		if err != nil {
			return runtimesessions.AdmitResult{}, err
		}
		if !created && original != req.Turn.TurnID {
			if err := runtimesessions.NoteReplay(state, req.Turn.IdempotencyKey, original); err != nil {
				return runtimesessions.AdmitResult{}, err
			}
			return runtimesessions.AdmitResult{Replayed: true, OriginalTurnID: original}, nil
		}
	}
	admitted, err := runtimesessions.Enqueue(state, &req.Turn, accepted.Sequence, req.MaxPending)
	if err != nil {
		if code, ok := runtime.CodeOf(err); ok && code == runtime.ErrorCodeRuntimeOverloaded {
			return runtimesessions.AdmitResult{Overloaded: true}, nil
		}
		return runtimesessions.AdmitResult{}, err
	}
	return runtimesessions.AdmitResult{
		Sequence:       accepted.Sequence,
		EventPayload:   accepted.Payload,
		EventCreatedAt: accepted.CreatedAt.UTC().Format(time.RFC3339Nano),
		ToStart:        admitted.ToStart,
	}, nil
}

func acceptedPayloadFor(desc *runtimesessions.Descriptor) []byte {
	if desc.AgentID != "" {
		return []byte(`{"origin":"` + desc.Origin + `","principal":"` + desc.PrincipalID + `","agent_id":"` + desc.AgentID + `"}`)
	}
	return []byte(`{"origin":"` + desc.Origin + `","principal":"` + desc.PrincipalID + `"}`)
}

func (s *stubSessionStore) Release(_ context.Context, sessionID, turnID string) (runtimesessions.ReleaseResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return runtimesessions.Release(s.stateLocked(sessionID), turnID)
}

func (s *stubSessionStore) Recover(_ context.Context, sessionID string, open, terminal []string) (runtimesessions.RecoverResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	restarted, found, err := runtimesessions.Recover(s.stateLocked(sessionID), open, terminal)
	if err != nil {
		return runtimesessions.RecoverResult{}, err
	}
	result := runtimesessions.RecoverResult{QueueDepth: len(s.states[sessionID].Queue)}
	if found {
		toStart := restarted
		result.ToStart = &toStart
		result.Found = true
	}
	return result, nil
}

func (s *stubSessionStore) Abort(_ context.Context, sessionID string) (runtimesessions.AbortResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dropped, err := runtimesessions.Abort(s.stateLocked(sessionID))
	if err != nil {
		return runtimesessions.AbortResult{}, err
	}
	return runtimesessions.AbortResult{Dropped: dropped}, nil
}

func newDurableTestRuntime(t *testing.T, cfg Config, executor TurnExecutor) (*Engine, *stubSessionStore, *sql.DB) {
	t.Helper()
	engine, db, events := newTestRuntime(t, cfg, executor)
	stub := newStubSessionStore(events, store.NewDedupeStore(db))
	engine.sessionStore = stub
	engine.MarkRecovered()
	return engine, stub, db
}

func newUnrecoveredDurableTestRuntime(t *testing.T, cfg Config, executor TurnExecutor) (*Engine, *stubSessionStore, *sql.DB) {
	t.Helper()
	engine, db, events := newTestRuntime(t, cfg, executor)
	stub := newStubSessionStore(events, store.NewDedupeStore(db))
	engine.sessionStore = stub
	return engine, stub, db
}
