package restate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	restate "github.com/restatedev/sdk-go"

	"github.com/anggasct/aura/internal/runtime"
	runtimesessions "github.com/anggasct/aura/internal/runtime/sessions"
	"github.com/anggasct/aura/internal/store"
)

const (
	SessionServiceName = "SessionTurns"

	SessionAdmitHandler   = "admit"
	SessionReleaseHandler = "release"
	SessionRecoverHandler = "recover"
	SessionAbortHandler   = "abort"
	SessionStatusHandler  = "status"

	sessionStateKey = "session_state"

	sessionDedupeWindow = 24 * time.Hour
)

type SessionStores struct {
	Events SessionEventStore
	Dedupe SessionDedupeStore
}

type SessionEventStore interface {
	Append(ctx context.Context, e *store.RuntimeEvent) error
	AppendSequenced(ctx context.Context, sessionID string, e *store.RuntimeEvent) (uint64, error)
	LastSequence(ctx context.Context, sessionID string) (uint64, error)
}

type SessionDedupeStore interface {
	Accept(ctx context.Context, source, externalID string, expiresAt time.Time, accepted *store.RuntimeEvent) (string, bool, error)
}

type sessionServer struct {
	events SessionEventStore
	dedupe SessionDedupeStore
	logger *slog.Logger
}

func buildSessionService(stores SessionStores, logger *slog.Logger) restate.ServiceDefinition {
	if logger == nil {
		logger = slog.Default()
	}
	server := &sessionServer{events: stores.Events, dedupe: stores.Dedupe, logger: logger}
	definition := restate.NewObject(SessionServiceName)
	definition.Handler(SessionAdmitHandler, restate.NewObjectHandler(server.admit))
	definition.Handler(SessionReleaseHandler, restate.NewObjectHandler(server.release))
	definition.Handler(SessionRecoverHandler, restate.NewObjectHandler(server.recover))
	definition.Handler(SessionAbortHandler, restate.NewObjectHandler(server.abort))
	definition.Handler(SessionStatusHandler, restate.NewObjectSharedHandler(server.status))
	return definition
}

func sessionTerminal(err error) error {
	return restate.ToTerminalError(err, restate.WithErrorCode(http.StatusBadRequest))
}

func loadSessionState(ctx restate.ObjectSharedContext) (*runtimesessions.State, error) {
	raw, err := restate.Get[json.RawMessage](ctx, sessionStateKey)
	if err != nil {
		return nil, err
	}
	return runtimesessions.DecodeState([]byte(raw))
}

func saveSessionState(ctx restate.ObjectContext, state *runtimesessions.State) {
	restate.Set(ctx, sessionStateKey, json.RawMessage(mustMarshalSessionState(state)))
}

func mustMarshalSessionState(state *runtimesessions.State) []byte {
	raw, err := json.Marshal(state)
	if err != nil {
		panic(fmt.Sprintf("marshal session state: %v", err))
	}
	return raw
}

func (s *sessionServer) admit(ctx restate.ObjectContext, req *runtimesessions.AdmitRequest) (runtimesessions.AdmitResult, error) {
	sessionID := restate.Key(ctx)
	if req.Turn.SessionID != "" && req.Turn.SessionID != sessionID {
		return runtimesessions.AdmitResult{}, sessionTerminal(fmt.Errorf("admit for session %q on object %q", req.Turn.SessionID, sessionID))
	}
	if req.Turn.TurnID == "" || req.AcceptedEventID == "" {
		return runtimesessions.AdmitResult{}, sessionTerminal(errors.New("admit requires a turn id and an accepted event id"))
	}
	if req.MaxPending <= 0 {
		return runtimesessions.AdmitResult{}, sessionTerminal(errors.New("admit requires a positive max pending bound"))
	}
	descriptor := req.Turn
	descriptor.SessionID = sessionID
	if err := descriptor.Validate(); err != nil {
		return runtimesessions.AdmitResult{}, sessionTerminal(err)
	}
	state, err := loadSessionState(ctx)
	if err != nil {
		return runtimesessions.AdmitResult{}, err
	}
	if original, err := runtimesessions.CheckReplay(state, descriptor.IdempotencyKey); err != nil {
		return runtimesessions.AdmitResult{}, sessionTerminal(err)
	} else if original != "" {
		return runtimesessions.AdmitResult{Replayed: true, OriginalTurnID: original}, nil
	}
	held := len(state.Queue)
	if state.Active != nil {
		held++
	}
	if held >= req.MaxPending {
		return runtimesessions.AdmitResult{Overloaded: true}, nil
	}
	accepted := &store.RuntimeEvent{
		ID:            req.AcceptedEventID,
		SessionID:     sessionID,
		TurnID:        descriptor.TurnID,
		Author:        descriptor.PrincipalID,
		Kind:          "turn.accepted",
		SchemaVersion: 1,
		Payload:       []byte(sessionAcceptedPayload(&descriptor)),
	}
	claimed, err := restate.Run(ctx, func(_ restate.RunContext) (sessionClaim, error) {
		return s.claim(ctx, &descriptor, accepted)
	}, restate.WithName("session-admit-claim"), restate.WithMaxRetryAttempts(1))
	if err != nil {
		return runtimesessions.AdmitResult{}, err
	}
	if claimed.replay {
		if err := runtimesessions.NoteReplay(state, descriptor.IdempotencyKey, claimed.original); err != nil {
			return runtimesessions.AdmitResult{}, sessionTerminal(err)
		}
		saveSessionState(ctx, state)
		return runtimesessions.AdmitResult{Replayed: true, OriginalTurnID: claimed.original}, nil
	}
	admitted, err := runtimesessions.Enqueue(state, &descriptor, accepted.Sequence, req.MaxPending)
	if err != nil {
		if code, ok := runtime.CodeOf(err); ok && code == runtime.ErrorCodeRuntimeOverloaded {
			return runtimesessions.AdmitResult{Overloaded: true}, nil
		}
		return runtimesessions.AdmitResult{}, sessionTerminal(err)
	}
	saveSessionState(ctx, state)
	return runtimesessions.AdmitResult{
		Sequence:       accepted.Sequence,
		EventPayload:   accepted.Payload,
		EventCreatedAt: accepted.CreatedAt.UTC().Format(time.RFC3339Nano),
		ToStart:        admitted.ToStart,
	}, nil
}

type sessionClaim struct {
	replay   bool
	original string
}

func (s *sessionServer) claim(ctx context.Context, descriptor *runtimesessions.Descriptor, accepted *store.RuntimeEvent) (sessionClaim, error) {
	accepted.CreatedAt = time.Now().UTC()
	if s.dedupe == nil || descriptor.IdempotencyKey == "" {
		if s.events == nil {
			return sessionClaim{}, errors.New("session admit requires an event store")
		}
		if _, err := s.events.AppendSequenced(ctx, descriptor.SessionID, accepted); err != nil {
			return sessionClaim{}, err
		}
		return sessionClaim{}, nil
	}
	original, created, err := s.dedupe.Accept(ctx, descriptor.Origin, descriptor.IdempotencyKey, time.Now().UTC().Add(sessionDedupeWindow), accepted)
	if err != nil {
		return sessionClaim{}, err
	}
	if !created && original != descriptor.TurnID {
		return sessionClaim{replay: true, original: original}, nil
	}
	return sessionClaim{}, nil
}

func sessionAcceptedPayload(descriptor *runtimesessions.Descriptor) string {
	if descriptor.AgentID != "" {
		return fmt.Sprintf(`{"origin":%q,"principal":%q,"agent_id":%q}`, descriptor.Origin, descriptor.PrincipalID, descriptor.AgentID)
	}
	return fmt.Sprintf(`{"origin":%q,"principal":%q}`, descriptor.Origin, descriptor.PrincipalID)
}

func (s *sessionServer) release(ctx restate.ObjectContext, req runtimesessions.ReleaseRequest) (runtimesessions.ReleaseResult, error) {
	if req.TurnID == "" {
		return runtimesessions.ReleaseResult{}, sessionTerminal(errors.New("release requires a turn id"))
	}
	state, err := loadSessionState(ctx)
	if err != nil {
		return runtimesessions.ReleaseResult{}, err
	}
	released, err := runtimesessions.Release(state, req.TurnID)
	if err != nil {
		return runtimesessions.ReleaseResult{}, sessionTerminal(err)
	}
	saveSessionState(ctx, state)
	return runtimesessions.ReleaseResult{ToStart: released.ToStart, QueueDepth: len(state.Queue)}, nil
}

func (s *sessionServer) recover(ctx restate.ObjectContext, req runtimesessions.RecoverRequest) (runtimesessions.RecoverResult, error) {
	state, err := loadSessionState(ctx)
	if err != nil {
		return runtimesessions.RecoverResult{}, err
	}
	restarted, found, err := runtimesessions.Recover(state, req.Open, req.Terminal)
	if err != nil {
		return runtimesessions.RecoverResult{}, sessionTerminal(err)
	}
	saveSessionState(ctx, state)
	result := runtimesessions.RecoverResult{QueueDepth: len(state.Queue)}
	if found {
		toStart := restarted
		result.ToStart = &toStart
		result.Found = true
	}
	return result, nil
}

func (s *sessionServer) abort(ctx restate.ObjectContext, _ json.RawMessage) (runtimesessions.AbortResult, error) {
	state, err := loadSessionState(ctx)
	if err != nil {
		return runtimesessions.AbortResult{}, err
	}
	dropped, err := runtimesessions.Abort(state)
	if err != nil {
		return runtimesessions.AbortResult{}, sessionTerminal(err)
	}
	saveSessionState(ctx, state)
	return runtimesessions.AbortResult{Dropped: dropped}, nil
}

func (s *sessionServer) status(ctx restate.ObjectSharedContext, _ json.RawMessage) (runtimesessions.StatusResult, error) {
	state, err := loadSessionState(ctx)
	if err != nil {
		return runtimesessions.StatusResult{}, err
	}
	result := runtimesessions.StatusResult{QueueDepth: len(state.Queue), LastSequence: state.LastSequence}
	if state.Active != nil {
		result.ActiveTurnID = state.Active.Descriptor.TurnID
		if !state.Active.Descriptor.Deadline.IsZero() {
			result.Deadline = state.Active.Descriptor.Deadline.UTC().Format(time.RFC3339Nano)
		}
	}
	return result, nil
}
