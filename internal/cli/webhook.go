package cli

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

	"github.com/anggasct/aura/internal/config"
	gatewaywebhook "github.com/anggasct/aura/internal/gateway/webhook"
	"github.com/anggasct/aura/internal/runtime"
	runtimeingress "github.com/anggasct/aura/internal/runtime/ingress"
	"github.com/anggasct/aura/internal/store"
)

type webhookDispatcher struct {
	mu         sync.Mutex
	sessions   store.SessionService
	executions store.WebhookExecutionStore
	backend    runtime.AgentRuntime
	retention  time.Duration
	clock      func() time.Time
	logger     *slog.Logger
	liveMu     sync.Mutex
	live       map[string]struct{}
}

func newWebhookDispatcher(sessions store.SessionService, executions store.WebhookExecutionStore, backend runtime.AgentRuntime, retention time.Duration, clock func() time.Time, logger *slog.Logger) (*webhookDispatcher, error) {
	if sessions == nil {
		return nil, errors.New("webhook dispatcher requires a session store")
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
	return &webhookDispatcher{
		sessions:   sessions,
		executions: executions,
		backend:    backend,
		retention:  retention,
		clock:      clock,
		logger:     logger,
		live:       make(map[string]struct{}),
	}, nil
}

func (d *webhookDispatcher) Dispatch(ctx context.Context, event *gatewaywebhook.AcceptedEvent) (gatewaywebhook.ExecutionRef, error) {
	if event == nil {
		return gatewaywebhook.ExecutionRef{}, gatewaywebhook.Errorf(gatewaywebhook.ErrorCodeInvalidArgument, "event must not be nil")
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	if ref, err := d.replayIdentical(ctx, event); err != nil || ref != (gatewaywebhook.ExecutionRef{}) {
		return ref, err
	}

	now := d.clock().UTC()
	executionID, err := newWebhookID("whex_")
	if err != nil {
		return gatewaywebhook.ExecutionRef{}, gatewaywebhook.Errorf(gatewaywebhook.ErrorCodeInvalidRequest, "execution identity could not be created")
	}
	turnID, err := newWebhookID("turn_")
	if err != nil {
		return gatewaywebhook.ExecutionRef{}, gatewaywebhook.Errorf(gatewaywebhook.ErrorCodeInvalidRequest, "execution identity could not be created")
	}
	sessionID, err := newWebhookID("sess_")
	if err != nil {
		return gatewaywebhook.ExecutionRef{}, gatewaywebhook.Errorf(gatewaywebhook.ErrorCodeInvalidRequest, "execution identity could not be created")
	}
	if err := d.createSession(ctx, sessionID, event.KeyID, now); err != nil {
		return gatewaywebhook.ExecutionRef{}, err
	}
	execution := &store.WebhookExecution{
		ID:         executionID,
		KeyID:      event.KeyID,
		EventID:    event.Envelope.EventID,
		Nonce:      event.Nonce,
		BodyDigest: event.BodyDigest,
		TurnID:     turnID,
		State:      store.WebhookExecutionStateAccepted,
		CreatedAt:  now,
		UpdatedAt:  now,
		ExpiresAt:  now.Add(d.retention),
	}
	if err := d.executions.InsertExecution(ctx, execution); err != nil {
		if code, ok := store.CodeOf(err); ok && code == store.ErrorCodeWebhookExecutionConflict {
			return d.replayIdentical(ctx, event)
		}
		return gatewaywebhook.ExecutionRef{}, gatewaywebhook.Errorf(gatewaywebhook.ErrorCodeInvalidRequest, "execution identity could not be created")
	}

	turn := webhookTurnRequest(sessionID, turnID, event)
	next, stop, err := startTurn(context.WithoutCancel(ctx), d.backend, turn)
	if err != nil {
		d.logger.WarnContext(ctx, "webhook runtime refused the turn",
			"component", "webhook",
			"key_id", event.KeyID,
			"error", err,
		)
		_ = d.executions.DeleteExecution(context.WithoutCancel(ctx), executionID)
		return gatewaywebhook.ExecutionRef{}, gatewaywebhook.Errorf(gatewaywebhook.ErrorCodeRuntimeOverloaded, "runtime did not accept the turn")
	}
	d.markLive(executionID)
	go d.drain(context.WithoutCancel(ctx), executionID, next, stop)

	return gatewaywebhook.ExecutionRef{ExecutionID: executionID, TurnID: turnID}, nil
}

func (d *webhookDispatcher) replayIdentical(ctx context.Context, event *gatewaywebhook.AcceptedEvent) (gatewaywebhook.ExecutionRef, error) {
	empty := gatewaywebhook.ExecutionRef{}
	previous, found, err := d.executions.ExecutionByNonce(ctx, event.KeyID, event.Nonce)
	if err != nil {
		return empty, gatewaywebhook.Errorf(gatewaywebhook.ErrorCodeInvalidRequest, "execution identity could not be read")
	}
	if !found {
		previous, found, err = d.executions.ExecutionByEvent(ctx, event.KeyID, event.Envelope.EventID)
		if err != nil {
			return empty, gatewaywebhook.Errorf(gatewaywebhook.ErrorCodeInvalidRequest, "execution identity could not be read")
		}
		if !found {
			return empty, nil
		}
	}
	if previous.BodyDigest != event.BodyDigest {
		return empty, gatewaywebhook.Errorf(gatewaywebhook.ErrorCodeReplayConflict, "identity was already used with different content")
	}
	if previous.State == store.WebhookExecutionStateAccepted && !d.isLive(previous.ID) {
		d.resubmitOrphaned(ctx, &previous, event)
	}
	return gatewaywebhook.ExecutionRef{ExecutionID: previous.ID, TurnID: previous.TurnID}, nil
}

func (d *webhookDispatcher) markLive(executionID string) {
	d.liveMu.Lock()
	defer d.liveMu.Unlock()
	d.live[executionID] = struct{}{}
}

func (d *webhookDispatcher) unmarkLive(executionID string) {
	d.liveMu.Lock()
	defer d.liveMu.Unlock()
	delete(d.live, executionID)
}

func (d *webhookDispatcher) isLive(executionID string) bool {
	d.liveMu.Lock()
	defer d.liveMu.Unlock()
	_, ok := d.live[executionID]
	return ok
}

func (d *webhookDispatcher) resubmitOrphaned(ctx context.Context, previous *store.WebhookExecution, event *gatewaywebhook.AcceptedEvent) {
	sessionID, err := newWebhookID("sess_")
	if err != nil {
		d.logger.WarnContext(ctx, "webhook orphan resubmission skipped",
			"component", "webhook",
			"key_id", event.KeyID,
			"error", err,
		)
		return
	}
	now := d.clock().UTC()
	if err := d.createSession(ctx, sessionID, event.KeyID, now); err != nil {
		d.logger.WarnContext(ctx, "webhook orphan resubmission skipped",
			"component", "webhook",
			"key_id", event.KeyID,
			"error", err,
		)
		return
	}
	turn := webhookTurnRequest(sessionID, previous.TurnID, event)
	next, stop, err := startTurn(context.WithoutCancel(ctx), d.backend, turn)
	if err != nil {
		d.logger.WarnContext(ctx, "webhook orphan resubmission skipped",
			"component", "webhook",
			"key_id", event.KeyID,
			"error", err,
		)
		return
	}
	d.markLive(previous.ID)
	go d.drain(context.WithoutCancel(ctx), previous.ID, next, stop)
}

func (d *webhookDispatcher) createSession(ctx context.Context, sessionID, keyID string, now time.Time) error {
	err := d.sessions.Create(ctx, &store.Session{
		ID:        sessionID,
		OwnerID:   "webhook:" + keyID,
		Metadata:  json.RawMessage(`{}`),
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		if code, ok := store.CodeOf(err); ok && code == store.ErrorCodeSessionIDConflict {
			return nil
		}
		return gatewaywebhook.Errorf(gatewaywebhook.ErrorCodeInvalidRequest, "execution identity could not be created")
	}
	return nil
}

func (d *webhookDispatcher) drain(ctx context.Context, executionID string, next turnStep, stop func()) {
	defer stop()
	defer d.unmarkLive(executionID)
	completedMessage := ""
	for {
		event, err, ok := next()
		if !ok {
			return
		}
		if err != nil {
			d.transition(ctx, executionID, store.WebhookExecutionStateFailed, "", string(runtime.ErrorCodeRuntimeInternal))
			return
		}
		switch event.Kind {
		case runtime.EventKindTurnAccepted:
		case runtime.EventKindMessageCompleted:
			completedMessage = event.ID
			d.transition(ctx, executionID, store.WebhookExecutionStateRunning, "", "")
		case runtime.EventKindTurnCompleted:
			d.transition(ctx, executionID, store.WebhookExecutionStateCompleted, firstNonEmpty(completedMessage, event.ID), "")
			return
		case runtime.EventKindTurnFailed:
			d.transition(ctx, executionID, store.WebhookExecutionStateFailed, firstNonEmpty(completedMessage, event.ID), failedEventCode(event.Payload))
			return
		case runtime.EventKindTurnCancelled:
			d.transition(ctx, executionID, store.WebhookExecutionStateCancelled, "", string(runtime.ErrorCodeTurnCancelled))
			return
		default:
			d.transition(ctx, executionID, store.WebhookExecutionStateRunning, "", "")
		}
	}
}

func (d *webhookDispatcher) transition(ctx context.Context, executionID, state, resultEventID, errorCode string) {
	now := d.clock().UTC()
	var err error
	switch state {
	case store.WebhookExecutionStateRunning:
		_, err = d.executions.MarkRunning(ctx, executionID, now)
	case store.WebhookExecutionStateCompleted, store.WebhookExecutionStateFailed, store.WebhookExecutionStateCancelled:
		_, err = d.executions.MarkTerminal(ctx, executionID, state, resultEventID, errorCode, now)
	default:
		d.logger.WarnContext(ctx, "webhook drain reached an unknown state",
			"component", "webhook",
			"state", state,
		)
		return
	}
	if err != nil {
		d.logger.WarnContext(ctx, "webhook state transition failed",
			"component", "webhook",
			"state", state,
			"error", err,
		)
	}
}

type turnStep func() (store.RuntimeEvent, error, bool)

func startTurn(ctx context.Context, backend runtime.AgentRuntime, turn *runtime.TurnRequest) (next turnStep, stop func(), err error) {
	pull, release := iter.Pull2(backend.Run(ctx, turn))
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
	next = func() (store.RuntimeEvent, error, bool) {
		if !resumed {
			resumed = true
			return event, nil, true
		}
		return pull()
	}
	return next, release, nil
}

func webhookTurnRequest(sessionID, turnID string, event *gatewaywebhook.AcceptedEvent) *runtime.TurnRequest {
	return &runtime.TurnRequest{
		TurnID:         turnID,
		SessionID:      sessionID,
		PrincipalID:    "webhook:" + event.KeyID,
		Origin:         runtime.OriginWebhook,
		Parts:          webhookTurnParts(event),
		IdempotencyKey: "webhook:" + event.KeyID + ":" + event.Envelope.EventID,
	}
}

func webhookTurnParts(event *gatewaywebhook.AcceptedEvent) []runtimeingress.InputPart {
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

func failedEventCode(payload []byte) string {
	var decoded struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil || decoded.Code == "" {
		return string(runtime.ErrorCodeRuntimeInternal)
	}
	return decoded.Code
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func newWebhookID(prefix string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(raw[:]), nil
}

type webhookStatusStore struct {
	executions store.WebhookExecutionStore
}

func (s *webhookStatusStore) ExecutionStatus(ctx context.Context, id string) (gatewaywebhook.ExecutionStatus, error) {
	execution, err := s.executions.Execution(ctx, id)
	if err != nil {
		if code, ok := store.CodeOf(err); ok && code == store.ErrorCodeWebhookExecutionNotFound {
			return gatewaywebhook.ExecutionStatus{}, gatewaywebhook.Errorf(gatewaywebhook.ErrorCodeExecutionNotFound, "unknown execution")
		}
		return gatewaywebhook.ExecutionStatus{}, err
	}
	return gatewaywebhook.ExecutionStatus{
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
	listen  string
	handler http.Handler
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
		return nil
	}
}

func buildWebhookListener(cfg *config.Config, db *sql.DB, backend runtime.AgentRuntime, logger *slog.Logger) (*webhookListener, error) {
	if cfg == nil {
		return nil, errors.New("webhook listener requires configuration")
	}
	if db == nil {
		return nil, errors.New("webhook listener requires storage")
	}
	if backend == nil {
		return nil, errors.New("webhook listener requires a runtime")
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
	executions := store.NewWebhookExecutionStore(db)
	sessions := store.NewSessionService(db)
	dispatcher, err := newWebhookDispatcher(sessions, executions, backend, time.Duration(cfg.Webhook.ReplayRetention), time.Now, logger)
	if err != nil {
		return nil, err
	}
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
	mux.Handle(gatewaywebhook.StatusPathPrefix, status)
	mux.Handle("/", handler)
	return &webhookListener{listen: cfg.Webhook.Listen, handler: mux}, nil
}

func webhookKeySource(envName string) (string, error) {
	value, ok := os.LookupEnv(envName)
	if !ok || value == "" {
		return "", fmt.Errorf("webhook: secret %s is unavailable", envName)
	}
	return value, nil
}
