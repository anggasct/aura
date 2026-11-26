package app

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	appconfig "github.com/anggasct/aura/internal/app/config"
	gatewaywebhook "github.com/anggasct/aura/internal/channels/webhook"
	"github.com/anggasct/aura/internal/modules/github"
	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
	platformwebhook "github.com/anggasct/aura/internal/platform/webhook"
)

type webhookDispatcher struct {
	mu         sync.Mutex
	sessions   turnstore.SessionService
	executions platformwebhook.ExecutionStore
	backend    turn.AgentRuntime
	retention  time.Duration
	clock      func() time.Time
	logger     *slog.Logger
	github     *github.Adapter
	reconciler *platformwebhook.Reconciler
	drains     sync.WaitGroup
}

func newWebhookDispatcher(sessions turnstore.SessionService, events platformwebhook.EventLister, executions platformwebhook.ExecutionStore, backend turn.AgentRuntime, retention time.Duration, clock func() time.Time, logger *slog.Logger) (*webhookDispatcher, error) {
	if sessions == nil {
		return nil, errors.New("webhook dispatcher requires a session store")
	}
	if events == nil {
		return nil, errors.New("webhook dispatcher requires an event lister")
	}
	if executions == nil {
		return nil, errors.New("webhook dispatcher requires an execution store")
	}
	if backend == nil {
		return nil, errors.New("webhook dispatcher requires a runtime")
	}
	if retention <= 0 {
		return nil, errors.New("webhook dispatcher requires a positive retention")
	}
	if clock == nil {
		return nil, errors.New("webhook dispatcher requires a clock")
	}
	if logger == nil {
		logger = slog.Default()
	}
	reconciler, err := platformwebhook.NewReconciler(&platformwebhook.ReconcilerDeps{
		Events:     events,
		Executions: executions,
		Clock:      clock,
		EventPage:  platformwebhook.DefaultReconcileEventPage,
	})
	if err != nil {
		return nil, err
	}
	return &webhookDispatcher{
		sessions:   sessions,
		executions: executions,
		backend:    backend,
		retention:  retention,
		clock:      clock,
		logger:     logger,
		reconciler: reconciler,
	}, nil
}

func (d *webhookDispatcher) reconcilePass() (*platformwebhook.ReconcilePass, error) {
	return platformwebhook.NewReconcilePass(&platformwebhook.ReconcilePassOptions{
		Executions: d.executions,
		Reconciler: d.reconciler,
		Clock:      d.clock,
		Batch:      platformwebhook.DefaultReconcileBatch,
		Interval:   platformwebhook.DefaultReconcileInterval,
		Logger:     d.logger,
	})
}

func (d *webhookDispatcher) Dispatch(ctx context.Context, event *platformwebhook.AcceptedEvent) (platformwebhook.ExecutionRef, error) {
	if event == nil {
		return platformwebhook.ExecutionRef{}, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidArgument, "event must not be nil")
	}
	if d.github != nil {
		handled, ref, err := d.github.Handle(ctx, event)
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

	turnRequest := webhookTurnRequest(sessionID, turnID, event)
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

func (d *webhookDispatcher) replayIdentical(ctx context.Context, event *platformwebhook.AcceptedEvent) (platformwebhook.ExecutionRef, error) {
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

func (d *webhookDispatcher) settleReplayed(ctx context.Context, previous *platformwebhook.Execution, event *platformwebhook.AcceptedEvent) {
	if err := d.reconcileReplayed(ctx, previous, event); err != nil {
		d.logger.WarnContext(ctx, "webhook replay reconcile failed",
			"component", "webhook",
			"key_id", event.KeyID,
			"error", err,
		)
	}
}

func (d *webhookDispatcher) reconcileReplayed(ctx context.Context, previous *platformwebhook.Execution, event *platformwebhook.AcceptedEvent) error {
	result, err := d.reconciler.Reconcile(ctx, previous)
	if err != nil || result.Admitted {
		return err
	}
	if err := d.createSession(ctx, previous.SessionID, event.KeyID, d.clock().UTC()); err != nil {
		return err
	}
	next, stop, err := startTurn(context.WithoutCancel(ctx), d.backend, webhookTurnRequest(previous.SessionID, previous.TurnID, event))
	if err != nil {
		return fmt.Errorf("submit the turn of webhook execution %s: %w", previous.ID, err)
	}
	d.startDrain(context.WithoutCancel(ctx), previous.ID, next, stop)
	return nil
}

func (d *webhookDispatcher) createSession(ctx context.Context, sessionID, keyID string, now time.Time) error {
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

func (d *webhookDispatcher) startDrain(ctx context.Context, executionID string, next turnStep, stop func()) {
	d.drains.Go(func() { d.drain(ctx, executionID, next, stop) })
}

func (d *webhookDispatcher) Wait(ctx context.Context) error {
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

func (d *webhookDispatcher) drain(ctx context.Context, executionID string, next turnStep, stop func()) {
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

func (d *webhookDispatcher) transition(ctx context.Context, executionID string, outcome *platformwebhook.ExecutionOutcome) {
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

func webhookTurnRequest(sessionID, turnID string, event *platformwebhook.AcceptedEvent) *turn.TurnRequest {
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

type webhookStatusStore struct {
	executions platformwebhook.ExecutionStore
}

func (s *webhookStatusStore) ExecutionStatus(ctx context.Context, id string) (platformwebhook.ExecutionStatus, error) {
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

type webhookListener struct {
	listen     string
	handler    http.Handler
	dispatcher *webhookDispatcher
}

func (l *webhookListener) Name() string { return "webhook" }

func (l *webhookListener) Start(ctx context.Context) error {
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", l.listen)
	if err != nil {
		return fmt.Errorf("webhook listener: bind %s: %w", l.listen, err)
	}
	server := &http.Server{
		Handler:           l.handler,
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("webhook listener: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("webhook listener shutdown: %w", err)
		}
		if err := l.dispatcher.Wait(shutdownCtx); err != nil {
			return fmt.Errorf("webhook listener drain: %w", err)
		}
		return nil
	}
}

type webhookListenerInput struct {
	cfg     *appconfig.Config
	db      *sql.DB
	backend turn.AgentRuntime
	logger  *slog.Logger
	runtime durable.Runtime
}

func buildWebhookListener(in *webhookListenerInput) (*webhookListener, error) {
	if in == nil {
		return nil, invalidOptions("webhook listener input must not be nil")
	}
	cfg, db, backend, logger := in.cfg, in.db, in.backend, in.logger
	if cfg == nil {
		return nil, errors.New("webhook listener requires configuration")
	}
	if db == nil {
		return nil, errors.New("webhook listener requires storage")
	}
	if backend == nil {
		return nil, errors.New("webhook listener requires a runtime")
	}
	if in.runtime == nil {
		return nil, errors.New("webhook listener requires a durable runtime")
	}
	if logger == nil {
		logger = slog.Default()
	}
	entries := make([]gatewaywebhook.KeyEntry, 0, len(cfg.Webhook.Keys))
	for _, key := range cfg.Webhook.Keys {
		var acceptUntil time.Time
		if key.AcceptUntil != "" {
			parsed, err := time.Parse(time.RFC3339, key.AcceptUntil)
			if err != nil {
				return nil, fmt.Errorf("webhook listener: key %q has an invalid rotation timestamp", key.ID)
			}
			acceptUntil = parsed
		}
		entries = append(entries, gatewaywebhook.KeyEntry{ID: key.ID, SecretEnv: key.SecretEnv, AcceptUntil: acceptUntil})
	}
	ring, err := gatewaywebhook.NewKeyRing(entries, webhookKeySource)
	if err != nil {
		return nil, err
	}
	limiter, err := gatewaywebhook.NewRateLimiter(cfg.Webhook.RequestsPerMinute, time.Now)
	if err != nil {
		return nil, err
	}
	executions, err := platformwebhook.NewSQLiteStore(db)
	if err != nil {
		return nil, err
	}
	sessions := turnstore.NewSessionService(db)
	turns := turnstore.NewTurnStore(db)
	dispatcher, err := newWebhookDispatcher(sessions, turns, executions, backend, time.Duration(cfg.Webhook.ReplayRetention), time.Now, logger)
	if err != nil {
		return nil, err
	}
	runs, err := workflow.NewStore(db, turns)
	if err != nil {
		return nil, err
	}
	githubAdapter, err := github.NewAdapter(runs, in.runtime, logger)
	if err != nil {
		return nil, err
	}
	dispatcher.github = githubAdapter
	handler, err := gatewaywebhook.NewHandler(gatewaywebhook.Settings{
		MaxBodySize:        int64(cfg.Webhook.MaxBodySize),
		TimestampTolerance: time.Duration(cfg.Webhook.TimestampTolerance),
	}, ring, limiter, time.Now, logger, dispatcher)
	if err != nil {
		return nil, err
	}
	status, err := gatewaywebhook.NewStatusHandler(ring, limiter, time.Now, time.Duration(cfg.Webhook.TimestampTolerance), logger, &webhookStatusStore{executions: executions})
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle(platformwebhook.StatusPathPrefix, status)
	mux.Handle("/", handler)
	return &webhookListener{listen: cfg.Webhook.Listen, handler: mux, dispatcher: dispatcher}, nil
}

func webhookKeySource(envName string) (string, error) {
	value, ok := os.LookupEnv(envName)
	if !ok || value == "" {
		return "", fmt.Errorf("webhook: secret %s is unavailable", envName)
	}
	return value, nil
}
