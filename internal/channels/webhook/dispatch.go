package webhook

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
	platformwebhook "github.com/anggasct/aura/internal/platform/webhook"
)

type SessionStore interface {
	Create(ctx context.Context, session *turn.Session) error
}

type ExecutionStore interface {
	platformwebhook.ExecutionStore
}

type EventHandler interface {
	Handle(ctx context.Context, event *platformwebhook.AcceptedEvent) (handled bool, ref platformwebhook.ExecutionRef, err error)
}

type DispatcherDeps struct {
	Sessions   SessionStore
	Events     platformwebhook.EventLister
	Executions ExecutionStore
	Runtime    turn.AgentRuntime
	Retention  time.Duration
	Clock      func() time.Time
	Logger     *slog.Logger
	Handlers   []EventHandler
}

type Dispatcher struct {
	mu         sync.Mutex
	sessions   SessionStore
	executions ExecutionStore
	backend    turn.AgentRuntime
	retention  time.Duration
	clock      func() time.Time
	logger     *slog.Logger
	handlers   []EventHandler
	reconciler *platformwebhook.Reconciler
	drains     sync.WaitGroup
}

var _ platformwebhook.Dispatcher = (*Dispatcher)(nil)

func NewDispatcher(deps *DispatcherDeps) (*Dispatcher, error) {
	if deps == nil {
		return nil, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidArgument, "dispatcher deps must not be nil")
	}
	if deps.Sessions == nil {
		return nil, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidArgument, "session store must not be nil")
	}
	if deps.Events == nil {
		return nil, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidArgument, "event lister must not be nil")
	}
	if deps.Executions == nil {
		return nil, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidArgument, "execution store must not be nil")
	}
	if deps.Runtime == nil {
		return nil, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidArgument, "runtime must not be nil")
	}
	if deps.Retention <= 0 {
		return nil, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidArgument, "replay retention must be positive")
	}
	if deps.Clock == nil {
		return nil, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidArgument, "clock must not be nil")
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	reconciler, err := platformwebhook.NewReconciler(&platformwebhook.ReconcilerDeps{
		Events:     deps.Events,
		Executions: deps.Executions,
		Clock:      deps.Clock,
		EventPage:  platformwebhook.DefaultReconcileEventPage,
	})
	if err != nil {
		return nil, err
	}
	return &Dispatcher{
		sessions:   deps.Sessions,
		executions: deps.Executions,
		backend:    deps.Runtime,
		retention:  deps.Retention,
		clock:      deps.Clock,
		logger:     logger,
		handlers:   slices.Clone(deps.Handlers),
		reconciler: reconciler,
	}, nil
}

func (d *Dispatcher) ReconcilePass() (*platformwebhook.ReconcilePass, error) {
	return platformwebhook.NewReconcilePass(&platformwebhook.ReconcilePassOptions{
		Executions: d.executions,
		Reconciler: d.reconciler,
		Clock:      d.clock,
		Batch:      platformwebhook.DefaultReconcileBatch,
		Interval:   platformwebhook.DefaultReconcileInterval,
		Logger:     d.logger,
	})
}

func (d *Dispatcher) Dispatch(ctx context.Context, event *platformwebhook.AcceptedEvent) (platformwebhook.ExecutionRef, error) {
	if event == nil {
		return platformwebhook.ExecutionRef{}, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidArgument, "event must not be nil")
	}
	for _, handler := range d.handlers {
		if handler == nil {
			continue
		}
		handled, ref, err := handler.Handle(ctx, event)
		if err != nil || handled {
			return ref, err
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	if ref, err := d.replayIdentical(ctx, event); err != nil || ref != (platformwebhook.ExecutionRef{}) {
		return ref, err
	}

	now := d.clock().UTC()
	executionID, err := newWebhookID("whex_")
	if err != nil {
		return platformwebhook.ExecutionRef{}, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidRequest, "execution identity could not be created")
	}
	turnID, err := newWebhookID("turn_")
	if err != nil {
		return platformwebhook.ExecutionRef{}, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidRequest, "execution identity could not be created")
	}
	sessionID, err := newWebhookID("sess_")
	if err != nil {
		return platformwebhook.ExecutionRef{}, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidRequest, "execution identity could not be created")
	}
	if err := d.createSession(ctx, sessionID, event.KeyID, now); err != nil {
		return platformwebhook.ExecutionRef{}, err
	}
	execution := &platformwebhook.Execution{
		ID:         executionID,
		KeyID:      event.KeyID,
		EventID:    event.Envelope.EventID,
		Nonce:      event.Nonce,
		BodyDigest: event.BodyDigest,
		TurnID:     turnID,
		SessionID:  sessionID,
		State:      platformwebhook.StateAccepted,
		CreatedAt:  now,
		UpdatedAt:  now,
		ExpiresAt:  now.Add(d.retention),
	}
	if err := d.executions.InsertExecution(ctx, execution); err != nil {
		if fault.HasCode(err, platformwebhook.ErrorCodeExecutionConflict) {
			return d.replayIdentical(ctx, event)
		}
		return platformwebhook.ExecutionRef{}, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidRequest, "execution identity could not be created")
	}

	turnRequest := TurnRequest(sessionID, turnID, event)
	next, stop, err := startTurn(context.WithoutCancel(ctx), d.backend, turnRequest)
	if err != nil {
		d.logger.WarnContext(ctx, "webhook runtime refused the turn",
			"component", "webhook",
			"key_id", event.KeyID,
			"error", err,
		)
		_ = d.executions.DeleteExecution(context.WithoutCancel(ctx), executionID)
		return platformwebhook.ExecutionRef{}, platformwebhook.Errorf(platformwebhook.ErrorCodeRuntimeOverloaded, "runtime did not accept the turn")
	}
	d.startDrain(context.WithoutCancel(ctx), executionID, next, stop)

	return platformwebhook.ExecutionRef{ExecutionID: executionID, TurnID: turnID}, nil
}

func (d *Dispatcher) replayIdentical(ctx context.Context, event *platformwebhook.AcceptedEvent) (platformwebhook.ExecutionRef, error) {
	empty := platformwebhook.ExecutionRef{}
	previous, found, err := d.executions.ExecutionByNonce(ctx, event.KeyID, event.Nonce)
	if err != nil {
		return empty, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidRequest, "execution identity could not be read")
	}
	if !found {
		previous, found, err = d.executions.ExecutionByEvent(ctx, event.KeyID, event.Envelope.EventID)
		if err != nil {
			return empty, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidRequest, "execution identity could not be read")
		}
		if !found {
			return empty, nil
		}
	}
	if previous.BodyDigest != event.BodyDigest {
		return empty, platformwebhook.Errorf(platformwebhook.ErrorCodeReplayConflict, "identity was already used with different content")
	}
	if previous.State == platformwebhook.StateAccepted || previous.State == platformwebhook.StateRunning {
		d.settleReplayed(ctx, &previous, event)
	}
	return platformwebhook.ExecutionRef{ExecutionID: previous.ID, TurnID: previous.TurnID}, nil
}

func (d *Dispatcher) settleReplayed(ctx context.Context, previous *platformwebhook.Execution, event *platformwebhook.AcceptedEvent) {
	if err := d.reconcileReplayed(ctx, previous, event); err != nil {
		d.logger.WarnContext(ctx, "webhook replay reconcile failed",
			"component", "webhook",
			"key_id", event.KeyID,
			"error", err,
		)
	}
}

func (d *Dispatcher) reconcileReplayed(ctx context.Context, previous *platformwebhook.Execution, event *platformwebhook.AcceptedEvent) error {
	result, err := d.reconciler.Reconcile(ctx, previous)
	if err != nil || result.Admitted {
		return err
	}
	if err := d.createSession(ctx, previous.SessionID, event.KeyID, d.clock().UTC()); err != nil {
		return err
	}
	next, stop, err := startTurn(context.WithoutCancel(ctx), d.backend, TurnRequest(previous.SessionID, previous.TurnID, event))
	if err != nil {
		return fmt.Errorf("submit the turn of webhook execution %s: %w", previous.ID, err)
	}
	d.startDrain(context.WithoutCancel(ctx), previous.ID, next, stop)
	return nil
}

func (d *Dispatcher) createSession(ctx context.Context, sessionID, keyID string, now time.Time) error {
	err := d.sessions.Create(ctx, &turn.Session{
		ID:        sessionID,
		OwnerID:   "webhook:" + keyID,
		Metadata:  json.RawMessage(`{}`),
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		if code, ok := fault.CodeOf[turnstore.ErrorCode](err); ok && code == turnstore.ErrorCodeSessionIDConflict {
			return nil
		}
		return platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidRequest, "execution identity could not be created")
	}
	return nil
}

func (d *Dispatcher) startDrain(ctx context.Context, executionID string, next turnStep, stop func()) {
	d.drains.Go(func() { d.drain(ctx, executionID, next, stop) })
}

func (d *Dispatcher) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		d.drains.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *Dispatcher) drain(ctx context.Context, executionID string, next turnStep, stop func()) {
	defer stop()
	var derivation platformwebhook.ExecutionDerivation
	for {
		event, err, ok := next()
		if !ok || err != nil {
			return
		}
		outcome, terminal := derivation.Next(&event)
		if terminal && event.ID == "" {
			return
		}
		d.transition(ctx, executionID, &outcome)
		if terminal {
			return
		}
	}
}

func (d *Dispatcher) transition(ctx context.Context, executionID string, outcome *platformwebhook.ExecutionOutcome) {
	if _, err := d.reconciler.Write(ctx, executionID, outcome); err != nil {
		d.logger.WarnContext(ctx, "webhook state transition failed",
			"component", "webhook",
			"state", outcome.State,
			"error", err,
		)
	}
}

type turnStep func() (turn.Event, error, bool)

func startTurn(ctx context.Context, backend turn.AgentRuntime, turnRequest *turn.TurnRequest) (next turnStep, stop func(), err error) {
	pull, release := iter.Pull2(backend.Run(ctx, turnRequest))
	event, pullErr, ok := pull()
	if !ok {
		release()
		return nil, nil, errors.New("runtime produced no admission result")
	}
	if pullErr != nil {
		release()
		return nil, nil, pullErr
	}
	resumed := false
	next = func() (turn.Event, error, bool) {
		if !resumed {
			resumed = true
			return event, nil, true
		}
		return pull()
	}
	return next, release, nil
}

func TurnRequest(sessionID, turnID string, event *platformwebhook.AcceptedEvent) *turn.TurnRequest {
	return &turn.TurnRequest{
		TurnID:         turnID,
		SessionID:      sessionID,
		PrincipalID:    "webhook:" + event.KeyID,
		PrincipalClass: principal.ClassTrusted,
		Origin:         turn.OriginWebhook,
		Parts:          webhookTurnParts(event),
		IdempotencyKey: "webhook:" + event.KeyID + ":" + event.Envelope.EventID,
	}
}

func webhookTurnParts(event *platformwebhook.AcceptedEvent) []runtimeingress.InputPart {
	parts := []runtimeingress.InputPart{
		{Text: event.Envelope.Subject},
		{Text: string(event.Envelope.Payload)},
	}
	if len(event.Envelope.Metadata) > 0 {
		if encoded, err := json.Marshal(event.Envelope.Metadata); err == nil {
			parts = append(parts, runtimeingress.InputPart{Text: string(encoded)})
		}
	}
	return parts
}

func newWebhookID(prefix string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(raw[:]), nil
}

type ExecutionStatusStore struct {
	executions ExecutionStore
}

var _ platformwebhook.StatusStore = (*ExecutionStatusStore)(nil)

func NewExecutionStatusStore(executions ExecutionStore) (*ExecutionStatusStore, error) {
	if executions == nil {
		return nil, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidArgument, "execution store must not be nil")
	}
	return &ExecutionStatusStore{executions: executions}, nil
}

func (s *ExecutionStatusStore) ExecutionStatus(ctx context.Context, id string) (platformwebhook.ExecutionStatus, error) {
	execution, err := s.executions.Execution(ctx, id)
	if err != nil {
		if fault.HasCode(err, platformwebhook.ErrorCodeExecutionNotFound) {
			return platformwebhook.ExecutionStatus{}, platformwebhook.Errorf(platformwebhook.ErrorCodeExecutionNotFound, "unknown execution")
		}
		return platformwebhook.ExecutionStatus{}, err
	}
	return platformwebhook.ExecutionStatus{
		ExecutionID:   execution.ID,
		TurnID:        execution.TurnID,
		State:         execution.State,
		ResultEventID: execution.ResultEventID,
		ErrorCode:     execution.ErrorCode,
		CreatedAt:     execution.CreatedAt,
		UpdatedAt:     execution.UpdatedAt,
		ExpiresAt:     execution.ExpiresAt,
	}, nil
}
