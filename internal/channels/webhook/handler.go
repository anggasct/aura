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

	"github.com/anggasct/aura/internal/platform/fault"
	platformwebhook "github.com/anggasct/aura/internal/platform/webhook"
)

const eventPath = "/webhook/event"

type Settings struct {
	MaxBodySize        int64
	TimestampTolerance time.Duration
}

type Handler struct {
	settings Settings
	keys     *KeyRing
	limiter  *RateLimiter
	clock    func() time.Time
	logger   *slog.Logger
	dispatch platformwebhook.Dispatcher
}

func NewHandler(settings Settings, keys *KeyRing, limiter *RateLimiter, clock func() time.Time, logger *slog.Logger, dispatch platformwebhook.Dispatcher) (*Handler, error) {
	if keys == nil {
		return nil, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidArgument, "key ring must not be nil")
	}
	if limiter == nil {
		return nil, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidArgument, "rate limiter must not be nil")
	}
	if clock == nil {
		return nil, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidArgument, "clock must not be nil")
	}
	if dispatch == nil {
		return nil, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidArgument, "dispatcher must not be nil")
	}
	if settings.MaxBodySize <= 0 {
		return nil, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidArgument, "max body size must be positive")
	}
	if settings.TimestampTolerance <= 0 {
		return nil, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidArgument, "timestamp tolerance must be positive")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{settings: settings, keys: keys, limiter: limiter, clock: clock, logger: logger, dispatch: dispatch}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if r.URL.Path != eventPath {
		writeError(w, http.StatusNotFound, platformwebhook.ErrorCodeInvalidRequest, "unknown route")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, platformwebhook.ErrorCodeInvalidRequest, "event endpoint accepts POST only")
		return
	}
	if !isJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, http.StatusUnsupportedMediaType, platformwebhook.ErrorCodeMediaUnsupported, "content type must be JSON")
		return
	}
	auth, err := ParseRequestAuth(r.Header)
	if err != nil {
		writeErrorTyped(w, err)
		return
	}
	if ok, _ := h.limiter.Allow(auth.KeyID, remoteAddr(r)); !ok {
		writeError(w, http.StatusTooManyRequests, platformwebhook.ErrorCodeRateLimited, "request budget exceeded")
		return
	}
	body, digest, err := readBoundedBody(r, h.settings.MaxBodySize)
	if err != nil {
		writeErrorTyped(w, err)
		return
	}
	if !h.authenticate(auth, body) || !auth.WithinTolerance(h.clock(), h.settings.TimestampTolerance) {
		writeError(w, http.StatusUnauthorized, platformwebhook.ErrorCodeAuthFailed, "authentication failed")
		h.logRejected(r.Context(), auth.KeyID, digest, string(platformwebhook.ErrorCodeAuthFailed), start)
		return
	}
	envelope, err := platformwebhook.ParseEnvelope(body)
	if err != nil {
		writeErrorTyped(w, err)
		h.logRejected(r.Context(), auth.KeyID, digest, string(codeOfOr(err, platformwebhook.ErrorCodeInvalidRequest)), start)
		return
	}
	ref, err := h.dispatch.Dispatch(r.Context(), &platformwebhook.AcceptedEvent{
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

func readBoundedBody(r *http.Request, limit int64) (body []byte, digest string, err error) {
	if r.ContentLength > limit {
		return nil, "", platformwebhook.Errorf(platformwebhook.ErrorCodeBodyTooLarge, "body exceeds the configured limit")
	}
	read, readErr := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if readErr != nil {
		return nil, "", platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidRequest, "body could not be read")
	}
	if int64(len(read)) > limit {
		return nil, "", platformwebhook.Errorf(platformwebhook.ErrorCodeBodyTooLarge, "body exceeds the configured limit")
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

func writeAccepted(w http.ResponseWriter, ref platformwebhook.ExecutionRef) {
	encoded, err := json.Marshal(acceptedBody{ExecutionID: ref.ExecutionID, TurnID: ref.TurnID, Status: "accepted"})
	if err != nil {
		http.Error(w, "response encoding failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Location", platformwebhook.StatusPathPrefix+ref.ExecutionID)
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write(encoded)
}

func writeErrorTyped(w http.ResponseWriter, err error) {
	code, ok := fault.CodeOf[platformwebhook.ErrorCode](err)
	if !ok {
		writeError(w, http.StatusBadRequest, platformwebhook.ErrorCodeInvalidRequest, "request was rejected")
		return
	}
	status, external := externalStatusForCode(code)
	if !external {
		writeError(w, http.StatusInternalServerError, platformwebhook.ErrorCodeInvalidRequest, "request was rejected")
		return
	}
	writeError(w, status, code, "request was rejected")
}

func dispatchError(err error) (int, platformwebhook.ErrorCode) {
	code, ok := fault.CodeOf[platformwebhook.ErrorCode](err)
	if !ok {
		return http.StatusInternalServerError, platformwebhook.ErrorCodeInvalidRequest
	}
	status, external := externalStatusForCode(code)
	if !external {
		return http.StatusInternalServerError, platformwebhook.ErrorCodeInvalidRequest
	}
	return status, code
}

func externalStatusForCode(code platformwebhook.ErrorCode) (int, bool) {
	switch code {
	case platformwebhook.ErrorCodeInvalidRequest:
		return http.StatusBadRequest, true
	case platformwebhook.ErrorCodeAuthFailed:
		return http.StatusUnauthorized, true
	case platformwebhook.ErrorCodeExecutionNotFound:
		return http.StatusNotFound, true
	case platformwebhook.ErrorCodeReplayConflict:
		return http.StatusConflict, true
	case platformwebhook.ErrorCodeBodyTooLarge:
		return http.StatusRequestEntityTooLarge, true
	case platformwebhook.ErrorCodeMediaUnsupported:
		return http.StatusUnsupportedMediaType, true
	case platformwebhook.ErrorCodeRateLimited:
		return http.StatusTooManyRequests, true
	case platformwebhook.ErrorCodeRuntimeOverloaded:
		return http.StatusServiceUnavailable, true
	default:
		return http.StatusInternalServerError, false
	}
}

func codeOfOr(err error, fallback platformwebhook.ErrorCode) platformwebhook.ErrorCode {
	if code, ok := fault.CodeOf[platformwebhook.ErrorCode](err); ok {
		return code
	}
	return fallback
}

func writeError(w http.ResponseWriter, status int, code platformwebhook.ErrorCode, message string) {
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
