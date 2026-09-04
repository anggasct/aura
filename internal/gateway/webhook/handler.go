package webhook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"time"
)

const (
	eventPath        = "/webhook/event"
	statusPathPrefix = "/webhook/executions/"
)

// Settings are the resolved runtime knobs of the webhook surface.
type Settings struct {
	MaxBodySize        int64
	TimestampTolerance time.Duration
}

// AcceptedEvent is one request that passed every enforcement gate up to and
// including strict JSON parsing. Body and digest travel together so the
// dispatch stage can persist replay identity without re-hashing.
type AcceptedEvent struct {
	KeyID      string
	Nonce      string
	BodyDigest string
	Body       []byte
	Envelope   Envelope
}

// ExecutionRef identifies the durable work created for an accepted event.
type ExecutionRef struct {
	ExecutionID string
	TurnID      string
}

// Dispatcher persists execution identity and submits the runtime turn for an
// authenticated event. It is the seam between request admission and durable
// execution; replay decisions and queue admission live behind it.
type Dispatcher interface {
	Dispatch(ctx context.Context, event *AcceptedEvent) (ExecutionRef, error)
}

// Handler serves the inbound webhook surface. Every request passes the
// gates in the frozen order: method, path, content type, header syntax,
// rate, body bound, authentication, timestamp window, then strict parsing.
// Nothing about a request body or its headers beyond identifiers, digest,
// and outcome reaches a log line.
type Handler struct {
	settings Settings
	keys     *KeyRing
	limiter  *RateLimiter
	clock    func() time.Time
	logger   *slog.Logger
	dispatch Dispatcher
}

// NewHandler wires the handler. A nil logger falls back to the default once,
// at construction; nil dependencies are construction errors, never runtime
// behavior.
func NewHandler(settings Settings, keys *KeyRing, limiter *RateLimiter, clock func() time.Time, logger *slog.Logger, dispatch Dispatcher) (*Handler, error) {
	if keys == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "key ring must not be nil")
	}
	if limiter == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "rate limiter must not be nil")
	}
	if clock == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "clock must not be nil")
	}
	if dispatch == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "dispatcher must not be nil")
	}
	if settings.MaxBodySize <= 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "max body size must be positive")
	}
	if settings.TimestampTolerance <= 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "timestamp tolerance must be positive")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{settings: settings, keys: keys, limiter: limiter, clock: clock, logger: logger, dispatch: dispatch}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if r.URL.Path != eventPath {
		writeError(w, http.StatusNotFound, ErrorCodeInvalidRequest, "unknown route")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, ErrorCodeInvalidRequest, "event endpoint accepts POST only")
		return
	}
	if !isJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, http.StatusUnsupportedMediaType, ErrorCodeMediaUnsupported, "content type must be JSON")
		return
	}
	auth, err := ParseRequestAuth(r.Header)
	if err != nil {
		writeErrorTyped(w, err)
		return
	}
	if ok, _ := h.limiter.Allow(auth.KeyID, remoteAddr(r)); !ok {
		writeError(w, http.StatusTooManyRequests, ErrorCodeRateLimited, "request budget exceeded")
		return
	}
	body, digest, err := readBoundedBody(r, h.settings.MaxBodySize)
	if err != nil {
		writeErrorTyped(w, err)
		return
	}
	if !h.authenticate(auth, body) || !auth.WithinTolerance(h.clock(), h.settings.TimestampTolerance) {
		writeError(w, http.StatusUnauthorized, ErrorCodeAuthFailed, "authentication failed")
		h.logRejected(r.Context(), auth.KeyID, digest, string(ErrorCodeAuthFailed), start)
		return
	}
	envelope, err := ParseEnvelope(body)
	if err != nil {
		writeErrorTyped(w, err)
		h.logRejected(r.Context(), auth.KeyID, digest, string(codeOfOr(err, ErrorCodeInvalidRequest)), start)
		return
	}
	ref, err := h.dispatch.Dispatch(r.Context(), &AcceptedEvent{
		KeyID:      auth.KeyID,
		Nonce:      auth.Nonce,
		BodyDigest: digest,
		Body:       body,
		Envelope:   envelope,
	})
	if err != nil {
		status, code := dispatchError(err)
		writeError(w, status, code, "request was not accepted")
		h.logRejected(r.Context(), auth.KeyID, digest, string(code), start)
		return
	}
	writeAccepted(w, ref)
	h.logAccepted(r.Context(), auth.KeyID, digest, start)
}

// authenticate resolves the key and verifies the canonical HMAC. Unknown
// key, expired key, and bad signature all fail identically.
func (h *Handler) authenticate(auth RequestAuth, body []byte) bool {
	secretValue, err := h.keys.Lookup(auth.KeyID, h.clock())
	if err != nil {
		return false
	}
	return VerifySignature(auth.Signature, secretValue, EventSigningBytes(auth.Timestamp, auth.Nonce, body))
}

func (h *Handler) logAccepted(ctx context.Context, keyID, digest string, start time.Time) {
	h.logger.InfoContext(ctx, "webhook request accepted",
		"component", "webhook",
		"key_id", keyID,
		"body_digest", digest,
		"status", "accepted",
		"latency_ms", time.Since(start).Milliseconds(),
	)
}

func (h *Handler) logRejected(ctx context.Context, keyID, digest, result string, start time.Time) {
	h.logger.InfoContext(ctx, "webhook request rejected",
		"component", "webhook",
		"key_id", keyID,
		"body_digest", digest,
		"status", result,
		"latency_ms", time.Since(start).Milliseconds(),
	)
}

// readBoundedBody reads at most one byte past the bound: the size error is
// raised the moment the limit is exceeded, before any parsing or dispatch.
func readBoundedBody(r *http.Request, limit int64) (body []byte, digest string, err error) {
	if r.ContentLength > limit {
		return nil, "", Errorf(ErrorCodeBodyTooLarge, "body exceeds the configured limit")
	}
	read, readErr := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if readErr != nil {
		return nil, "", Errorf(ErrorCodeInvalidRequest, "body could not be read")
	}
	if int64(len(read)) > limit {
		return nil, "", Errorf(ErrorCodeBodyTooLarge, "body exceeds the configured limit")
	}
	sum := sha256.Sum256(read)
	return read, hex.EncodeToString(sum[:]), nil
}

func isJSONContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return false
	}
	if mediaType == "application/json" {
		return true
	}
	const suffix = "+json"
	return len(mediaType) > len(suffix) && mediaType[len(mediaType)-len(suffix):] == suffix
}

func remoteAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

type acceptedBody struct {
	ExecutionID string `json:"execution_id"`
	TurnID      string `json:"turn_id"`
	Status      string `json:"status"`
}

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeAccepted(w http.ResponseWriter, ref ExecutionRef) {
	encoded, err := json.Marshal(acceptedBody{ExecutionID: ref.ExecutionID, TurnID: ref.TurnID, Status: "accepted"})
	if err != nil {
		http.Error(w, "response encoding failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Location", statusPathPrefix+ref.ExecutionID)
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write(encoded)
}

func writeErrorTyped(w http.ResponseWriter, err error) {
	code, ok := CodeOf(err)
	if !ok {
		code = ErrorCodeInvalidRequest
	}
	writeError(w, statusForCode(code), code, "request was rejected")
}

func dispatchError(err error) (int, ErrorCode) {
	code, ok := CodeOf(err)
	if !ok {
		return http.StatusInternalServerError, ErrorCodeInvalidRequest
	}
	return statusForCode(code), code
}

func statusForCode(code ErrorCode) int {
	switch code {
	case ErrorCodeAuthFailed:
		return http.StatusUnauthorized
	case ErrorCodeReplayConflict:
		return http.StatusConflict
	case ErrorCodeBodyTooLarge:
		return http.StatusRequestEntityTooLarge
	case ErrorCodeMediaUnsupported:
		return http.StatusUnsupportedMediaType
	case ErrorCodeRateLimited:
		return http.StatusTooManyRequests
	case ErrorCodeRuntimeOverloaded:
		return http.StatusServiceUnavailable
	case ErrorCodeExecutionNotFound:
		return http.StatusNotFound
	default:
		return http.StatusBadRequest
	}
}

func codeOfOr(err error, fallback ErrorCode) ErrorCode {
	if code, ok := CodeOf(err); ok {
		return code
	}
	return fallback
}

func writeError(w http.ResponseWriter, status int, code ErrorCode, message string) {
	var body errorBody
	body.Error.Code = string(code)
	body.Error.Message = message
	encoded, err := json.Marshal(body)
	if err != nil {
		http.Error(w, "response encoding failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}
