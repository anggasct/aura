package httpchat

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/principal"
)

const (
	routeModels          = "/v1/models"
	routeChatCompletions = "/v1/chat/completions"

	authorizationHeader = "Authorization"
	bearerScheme        = "Bearer"
	contentTypeJSON     = "application/json"
	overloadRetryAfter  = "1"
)

type Key struct {
	ID        string
	Digest    string
	PersonaID string
	Class     principal.Class
	CreatedAt time.Time
	Revoked   bool
}

type KeyStore interface {
	GetByID(ctx context.Context, id string) (Key, bool, error)
}

type Principal struct {
	ID           string
	Class        principal.Class
	PersonaID    string
	KeyID        string
	KeyCreatedAt time.Time
}

type Call struct {
	Principal Principal
	Body      []byte
}

type ChatHandler interface {
	ServeChat(w http.ResponseWriter, r *http.Request, call *Call)
}

type Options struct {
	Keys                KeyStore
	Chat                ChatHandler
	MaxBodyBytes        int64
	RequestsPerMinute   int
	MaxConcurrentPerKey int
	Logger              *slog.Logger
}

type route struct {
	method string
	serve  func(w http.ResponseWriter, r *http.Request, caller *Principal)
}

type Handler struct {
	keys         KeyStore
	routes       map[string]route
	maxBodyBytes int64
	limiter      *rateLimiter
	slots        *slotTable
	logger       *slog.Logger
}

func NewHandler(opts *Options) (*Handler, error) {
	switch {
	case opts == nil:
		return nil, Errorf(ErrorCodeInvalidArgument, "handler options must not be nil")
	case opts.Keys == nil:
		return nil, Errorf(ErrorCodeInvalidArgument, "key store must not be nil")
	case opts.MaxBodyBytes < 1:
		return nil, Errorf(ErrorCodeInvalidArgument, "body bound must be positive")
	case opts.RequestsPerMinute < 1:
		return nil, Errorf(ErrorCodeInvalidArgument, "request rate must be positive")
	case opts.MaxConcurrentPerKey < 1:
		return nil, Errorf(ErrorCodeInvalidArgument, "concurrency limit must be positive")
	case opts.Logger == nil:
		return nil, Errorf(ErrorCodeInvalidArgument, "logger must not be nil")
	}
	h := &Handler{
		keys:         opts.Keys,
		routes:       map[string]route{routeModels: {method: http.MethodGet, serve: serveModels}},
		maxBodyBytes: opts.MaxBodyBytes,
		limiter:      newRateLimiter(opts.RequestsPerMinute),
		slots:        newSlotTable(opts.MaxConcurrentPerKey),
		logger:       opts.Logger,
	}
	if opts.Chat != nil {
		chat := opts.Chat
		h.routes[routeChatCompletions] = route{
			method: http.MethodPost,
			serve:  func(w http.ResponseWriter, r *http.Request, caller *Principal) { h.serveChat(chat, w, r, caller) },
		}
	}
	return h, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	target, mounted := h.routes[r.URL.Path]
	if !mounted {
		h.reject(ctx, w, &errNotFound, "route")
		return
	}
	if r.Method != target.method {
		w.Header().Set("Allow", target.method)
		h.reject(ctx, w, &errMethodNotAllowed, "method")
		return
	}
	key, spec, reason := h.authenticate(r)
	if spec != nil {
		h.reject(ctx, w, spec, reason)
		return
	}
	if !h.slots.acquire(key.ID) {
		w.Header().Set("Retry-After", overloadRetryAfter)
		h.reject(ctx, w, &errRuntimeOverloaded, "concurrency")
		return
	}
	defer h.slots.release(key.ID)
	if allowed, wait := h.limiter.allow(key.ID, time.Now()); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait/time.Second)))
		h.reject(ctx, w, &errRateLimited, "rate")
		return
	}
	caller := principalFor(&key)
	target.serve(w, r, &caller)
}

func (h *Handler) authenticate(r *http.Request) (Key, *errorSpec, string) {
	presented, ok := bearerToken(r)
	if !ok {
		return Key{}, &errInvalidAPIKey, "missing_key"
	}
	id, err := ParseKey(presented)
	if err != nil {
		return Key{}, &errInvalidAPIKey, "malformed_key"
	}
	key, found, err := h.keys.GetByID(r.Context(), id)
	switch {
	case err != nil:
		h.logger.ErrorContext(r.Context(), "http api key lookup failed", "component", "http", "error", err)
		return Key{}, &errInternal, "key_lookup"
	case !found:
		return Key{}, &errInvalidAPIKey, "unknown_key"
	case !VerifyKey(presented, key.Digest):
		return Key{}, &errInvalidAPIKey, "digest_mismatch"
	case key.Revoked:
		return Key{}, &errInvalidAPIKey, "revoked_key"
	}
	return key, nil, ""
}

func bearerToken(r *http.Request) (string, bool) {
	values := r.Header.Values(authorizationHeader)
	if len(values) != 1 {
		return "", false
	}
	scheme, token, found := strings.Cut(values[0], " ")
	if !found || !strings.EqualFold(scheme, bearerScheme) || token == "" {
		return "", false
	}
	return token, true
}

func principalFor(key *Key) Principal {
	id := principal.Namespaced(principal.ChannelHTTP, key.ID)
	if key.Class == principal.ClassOwner {
		id = principal.OwnerID
	}
	return Principal{ID: id, Class: key.Class, PersonaID: key.PersonaID, KeyID: key.ID, KeyCreatedAt: key.CreatedAt}
}

func (h *Handler) serveChat(chat ChatHandler, w http.ResponseWriter, r *http.Request, caller *Principal) {
	ctx := r.Context()
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != contentTypeJSON {
		h.reject(ctx, w, &errUnsupportedMediaType, "content_type")
		return
	}
	if r.ContentLength > h.maxBodyBytes {
		h.reject(ctx, w, &errRequestTooLarge, "body_too_large")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.reject(ctx, w, &errRequestTooLarge, "body_too_large")
			return
		}
		h.reject(ctx, w, &errInvalidRequest, "body_unreadable")
		return
	}
	chat.ServeChat(w, r, &Call{Principal: *caller, Body: body})
}

func (h *Handler) reject(ctx context.Context, w http.ResponseWriter, spec *errorSpec, reason string) {
	h.logger.WarnContext(ctx, "http request rejected", "component", "http", "reason", reason, "status", spec.status)
	writeError(w, spec)
}

func serveModels(w http.ResponseWriter, _ *http.Request, caller *Principal) {
	writeJSON(w, http.StatusOK, &modelList{
		Object: "list",
		Data:   []model{{ID: caller.PersonaID, Object: "model", Created: caller.KeyCreatedAt.Unix(), OwnedBy: "aura"}},
	})
}

type modelList struct {
	Object string  `json:"object"`
	Data   []model `json:"data"`
}

type model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}
