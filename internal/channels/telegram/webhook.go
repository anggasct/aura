package telegram

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/secret"
)

const (
	webhookRoutePrefix   = "/telegram/"
	webhookAuthHeader    = "X-Telegram-Bot-Api-Secret-Token"
	webhookSecretMaxLen  = 256
	webhookCallTimeout   = 30 * time.Second
	webhookFailureWindow = 10 * time.Minute
	webhookHeaderTimeout = 5 * time.Second
	webhookReadTimeout   = 30 * time.Second
	webhookIdleTimeout   = time.Minute
	webhookCancelGrace   = 5 * time.Second
)

const (
	findingWebhookMismatch = "telegram_webhook_mismatch"
	findingWebhookFailing  = "telegram_webhook_failing"
)

var errWebhookSecretInvalid = Errorf(ErrorCodeInvalidArgument, "webhook secret is unavailable or invalid")

type webhookSession struct {
	digest [sha256.Size]byte
}

func (s *webhookSession) matches(values []string) bool {
	if len(values) != 1 {
		return false
	}
	presented := sha256.Sum256([]byte(values[0]))
	return subtle.ConstantTimeCompare(presented[:], s.digest[:]) == 1
}

func webhookSettings(cfg *config.Telegram, account *config.TelegramAccount) (secret.Reference, chan struct{}, error) {
	switch account.Mode {
	case "", config.TelegramModePolling:
		return secret.Reference{}, nil, nil
	case config.TelegramModeWebhook:
	default:
		return secret.Reference{}, nil, Errorf(ErrorCodeInvalidArgument, "telegram account mode must be polling or webhook")
	}
	ref, err := parseSecretReference(account.WebhookSecretRef, "webhook secret")
	if err != nil {
		return secret.Reference{}, nil, err
	}
	hook := &cfg.Webhook
	switch {
	case hook.PublicBaseURL == "":
		return secret.Reference{}, nil, Errorf(ErrorCodeInvalidArgument, "webhook public base url must not be empty")
	case hook.MaxBodyBytes < 1:
		return secret.Reference{}, nil, Errorf(ErrorCodeInvalidArgument, "webhook body bound must be positive")
	case hook.MaxConnections < 1:
		return secret.Reference{}, nil, Errorf(ErrorCodeInvalidArgument, "webhook connection cap must be positive")
	}
	return ref, make(chan struct{}, hook.MaxConnections), nil
}

func (a *Adapter) webhookPath() string {
	return webhookRoutePrefix + a.account.Name
}

func (a *Adapter) webhookURL() string {
	return strings.TrimRight(a.cfg.Webhook.PublicBaseURL, "/") + a.webhookPath()
}

func (a *Adapter) webhookSecret() (string, error) {
	value, err := a.webhookRef.Resolve()
	if err != nil {
		return "", errWebhookSecretInvalid
	}
	value = strings.TrimSpace(value)
	if !validWebhookSecret(value) {
		return "", errWebhookSecretInvalid
	}
	return value, nil
}

func validWebhookSecret(value string) bool {
	if value == "" || len(value) > webhookSecretMaxLen {
		return false
	}
	for i := range len(value) {
		c := value[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

func (a *Adapter) webhookLoop(ctx context.Context) {
	attempt := 0
	for {
		err := a.registerWebhook(ctx)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return
		}
		a.markFault(classifyFault(err))
		a.logger.WarnContext(ctx, "telegram webhook registration failed", "component", "telegram", "account", a.account.Name, "error", err)
		if !sleepContext(ctx, a.retryDelay(attempt, err)) {
			return
		}
		attempt++
	}
	if attempt > 0 {
		a.refreshIdentity()
	}
	<-ctx.Done()
}

func (a *Adapter) registerWebhook(ctx context.Context) error {
	secretToken, err := a.webhookSecret()
	if err != nil {
		a.webhook.Store(nil)
		return err
	}
	a.webhook.Store(&webhookSession{digest: sha256.Sum256([]byte(secretToken))})
	token, ok := a.botToken()
	if !ok {
		return errTokenUnavailable
	}
	call, cancel := context.WithTimeout(ctx, webhookCallTimeout)
	defer cancel()
	err = a.bot.setWebhook(call, token, &setWebhookRequest{
		URL:            a.webhookURL(),
		SecretToken:    secretToken,
		AllowedUpdates: allowedUpdates(),
		MaxConnections: a.cfg.Webhook.MaxConnections,
	})
	if err != nil {
		return err
	}
	a.markLive()
	return nil
}

func (a *Adapter) webhookFindings(ctx context.Context) []health.Finding {
	token, ok := a.botToken()
	if !ok {
		return []health.Finding{a.finding(faultBotRefUnresolved, faultDetails[faultBotRefUnresolved])}
	}
	call, cancel := context.WithTimeout(ctx, webhookCallTimeout)
	defer cancel()
	info, err := a.bot.getWebhookInfo(call, token)
	if err != nil {
		fault := classifyFault(err)
		return []health.Finding{a.finding(fault, faultDetails[fault])}
	}
	if info.URL != a.webhookURL() {
		return []health.Finding{a.finding(findingWebhookMismatch, "telegram holds a different webhook url than the configured one")}
	}
	if info.LastErrorDate > 0 && time.Since(time.Unix(info.LastErrorDate, 0)) <= webhookFailureWindow {
		return []health.Finding{a.finding(findingWebhookFailing, "telegram reported a recent webhook delivery error")}
	}
	return nil
}

func (a *Adapter) noteWebhookFailure() {
	if adapterState(a.state.Load()) == stateLive {
		a.markFault(faultIntake)
	}
}

func (a *Adapter) noteWebhookSuccess() {
	if a.currentFault() == faultIntake {
		a.markLive()
	}
}

func (a *Adapter) reject(ctx context.Context, w http.ResponseWriter, status int, reason string) {
	a.logger.WarnContext(ctx, "telegram webhook request rejected", "component", "telegram", "account", a.account.Name, "reason", reason, "status", status)
	switch status {
	case http.StatusMethodNotAllowed:
		w.Header().Set("Allow", http.MethodPost)
	case http.StatusServiceUnavailable:
		w.Header().Set("Retry-After", "1")
	}
	w.WriteHeader(status)
}

func (a *Adapter) serveWebhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method != http.MethodPost {
		a.reject(ctx, w, http.StatusMethodNotAllowed, "method")
		return
	}
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
		a.reject(ctx, w, http.StatusUnsupportedMediaType, "content_type")
		return
	}
	session := a.webhook.Load()
	if session == nil {
		a.reject(ctx, w, http.StatusServiceUnavailable, "not_ready")
		return
	}
	if !session.matches(r.Header.Values(webhookAuthHeader)) {
		a.reject(ctx, w, http.StatusUnauthorized, "unauthorized")
		return
	}
	select {
	case a.webhookSlots <- struct{}{}:
		defer func() { <-a.webhookSlots }()
	default:
		a.reject(ctx, w, http.StatusServiceUnavailable, "busy")
		return
	}
	limit := a.cfg.Webhook.MaxBodyBytes
	if r.ContentLength > limit {
		a.reject(ctx, w, http.StatusRequestEntityTooLarge, "body_too_large")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			a.reject(ctx, w, http.StatusRequestEntityTooLarge, "body_too_large")
			return
		}
		a.reject(ctx, w, http.StatusBadRequest, "body_unreadable")
		return
	}
	item, ok := decodeWebhookUpdate(raw)
	if !ok {
		a.reject(ctx, w, http.StatusBadRequest, "malformed")
		return
	}
	if err := a.handleUpdate(ctx, item); err != nil || ctx.Err() != nil {
		if ctx.Err() == nil {
			a.noteWebhookFailure()
		}
		a.logger.WarnContext(ctx, "telegram webhook update failed", "component", "telegram", "account", a.account.Name, "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	a.noteWebhookSuccess()
	w.WriteHeader(http.StatusOK)
}

func decodeWebhookUpdate(raw []byte) (*update, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var item update
	if err := decoder.Decode(&item); err != nil {
		return nil, false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	if item.UpdateID <= 0 {
		return nil, false
	}
	return &item, true
}

type WebhookHandler struct {
	routes   map[string]*Adapter
	mu       sync.Mutex
	draining bool
	inflight sync.WaitGroup
}

func NewWebhookHandler(adapters []*Adapter) (*WebhookHandler, error) {
	routes := make(map[string]*Adapter, len(adapters))
	for _, adapter := range adapters {
		if adapter == nil {
			return nil, Errorf(ErrorCodeInvalidArgument, "telegram adapter must not be nil")
		}
		if !adapter.account.WebhookMode() {
			continue
		}
		path := adapter.webhookPath()
		if _, duplicate := routes[path]; duplicate {
			return nil, Errorf(ErrorCodeInvalidArgument, "telegram webhook route is configured more than once")
		}
		routes[path] = adapter
	}
	if len(routes) == 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "no telegram account uses webhook mode")
	}
	return &WebhookHandler{routes: routes}, nil
}

func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	adapter, ok := h.routes[r.URL.Path]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if !h.enter() {
		adapter.reject(r.Context(), w, http.StatusServiceUnavailable, "draining")
		return
	}
	defer h.inflight.Done()
	adapter.serveWebhook(w, r)
}

func (h *WebhookHandler) enter() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.draining {
		return false
	}
	h.inflight.Add(1)
	return true
}

func (h *WebhookHandler) drain() {
	h.mu.Lock()
	h.draining = true
	h.mu.Unlock()
	h.inflight.Wait()
}

type WebhookServer struct {
	handler *WebhookHandler
	listen  string
	grace   time.Duration
}

func NewWebhookServer(handler *WebhookHandler, listen string, grace time.Duration) (*WebhookServer, error) {
	if handler == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "webhook handler must not be nil")
	}
	if listen == "" {
		return nil, Errorf(ErrorCodeInvalidArgument, "webhook listen address must not be empty")
	}
	if grace <= 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "webhook shutdown grace must be positive")
	}
	return &WebhookServer{handler: handler, listen: listen, grace: grace}, nil
}

func (s *WebhookServer) Name() string { return "telegram-webhook" }

func (s *WebhookServer) Start(ctx context.Context) error {
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.listen)
	if err != nil {
		return &Error{Code: ErrorCodeConnectionFailed, Detail: "telegram webhook listener is not bindable", Cause: err}
	}
	return s.Serve(ctx, listener)
}

func (s *WebhookServer) Serve(ctx context.Context, listener net.Listener) error {
	deliveries, cancelDeliveries := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelDeliveries()
	server := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: webhookHeaderTimeout,
		ReadTimeout:       webhookReadTimeout,
		IdleTimeout:       webhookIdleTimeout,
		BaseContext:       func(net.Listener) context.Context { return deliveries },
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	var serveErr error
	select {
	case serveErr = <-served:
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.grace)
	defer cancel()
	shutdownErr := server.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		cancelDeliveries()
		cancelCtx, cancelGrace := context.WithTimeout(context.WithoutCancel(ctx), webhookCancelGrace)
		defer cancelGrace()
		if server.Shutdown(cancelCtx) != nil {
			_ = server.Close()
		}
	}
	if serveErr == nil {
		serveErr = <-served
	}
	s.handler.drain()
	switch {
	case shutdownErr != nil:
		return &Error{Code: ErrorCodeConnectionFailed, Detail: "telegram webhook listener did not drain in time", Cause: shutdownErr}
	case serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed):
		return &Error{Code: ErrorCodeConnectionFailed, Detail: "telegram webhook listener stopped", Cause: serveErr}
	default:
		return nil
	}
}
