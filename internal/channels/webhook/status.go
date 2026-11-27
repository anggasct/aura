package webhook

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	platformwebhook "github.com/anggasct/aura/internal/platform/webhook"
)

type StatusHandler struct {
	keys      *KeyRing
	limiter   *RateLimiter
	clock     func() time.Time
	tolerance time.Duration
	logger    *slog.Logger
	status    platformwebhook.StatusStore
}

func NewStatusHandler(keys *KeyRing, limiter *RateLimiter, clock func() time.Time, tolerance time.Duration, logger *slog.Logger, status platformwebhook.StatusStore) (*StatusHandler, error) {
	if keys == nil {
		return nil, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidArgument, "key ring must not be nil")
	}
	if limiter == nil {
		return nil, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidArgument, "rate limiter must not be nil")
	}
	if clock == nil {
		return nil, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidArgument, "clock must not be nil")
	}
	if tolerance <= 0 {
		return nil, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidArgument, "timestamp tolerance must be positive")
	}
	if status == nil {
		return nil, platformwebhook.Errorf(platformwebhook.ErrorCodeInvalidArgument, "status store must not be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &StatusHandler{keys: keys, limiter: limiter, clock: clock, tolerance: tolerance, logger: logger, status: status}, nil
}

func (h *StatusHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, platformwebhook.ErrorCodeInvalidRequest, "status endpoint accepts GET only")
		return
	}
	path := r.URL.EscapedPath()
	id, ok := strings.CutPrefix(path, platformwebhook.StatusPathPrefix)
	if !ok || id == "" || strings.Contains(id, "/") {
		writeError(w, http.StatusNotFound, platformwebhook.ErrorCodeExecutionNotFound, "unknown execution")
		return
	}
	auth, err := ParseRequestAuth(r.Header)
	if err != nil {
		writeErrorTyped(w, err)
		return
	}
	if allowed, _ := h.limiter.Allow(auth.KeyID, remoteAddr(r)); !allowed {
		writeError(w, http.StatusTooManyRequests, platformwebhook.ErrorCodeRateLimited, "request budget exceeded")
		return
	}
	if !h.authenticate(auth, path) || !auth.WithinTolerance(h.clock(), h.tolerance) {
		writeError(w, http.StatusUnauthorized, platformwebhook.ErrorCodeAuthFailed, "authentication failed")
		return
	}
	execution, err := h.status.ExecutionStatus(r.Context(), id)
	if err != nil {
		if code, ok := fault.CodeOf[platformwebhook.ErrorCode](err); ok && code == platformwebhook.ErrorCodeExecutionNotFound {
			writeError(w, http.StatusNotFound, platformwebhook.ErrorCodeExecutionNotFound, "unknown execution")
			return
		}
		writeError(w, http.StatusInternalServerError, platformwebhook.ErrorCodeInvalidRequest, "status is temporarily unavailable")
		return
	}
	if !execution.ExpiresAt.IsZero() && !h.clock().Before(execution.ExpiresAt) {
		writeError(w, http.StatusNotFound, platformwebhook.ErrorCodeExecutionNotFound, "unknown execution")
		return
	}
	writeStatus(w, &execution)
	h.logger.InfoContext(r.Context(), "webhook status served",
		"component", "webhook",
		"key_id", auth.KeyID,
		"status", "served",
		"latency_ms", time.Since(start).Milliseconds(),
	)
}

func (h *StatusHandler) authenticate(auth RequestAuth, path string) bool {
	secretValue, err := h.keys.Lookup(auth.KeyID, h.clock())
	if err != nil {
		return false
	}
	return VerifySignature(auth.Signature, secretValue, StatusSigningBytes(auth.Timestamp, auth.Nonce, path))
}

type statusBody struct {
	ExecutionID   string  `json:"execution_id"`
	TurnID        string  `json:"turn_id"`
	Status        string  `json:"status"`
	ResultEventID *string `json:"result_event_id"`
	ErrorCode     *string `json:"error_code"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`
}

func writeStatus(w http.ResponseWriter, execution *platformwebhook.ExecutionStatus) {
	body := statusBody{
		ExecutionID: execution.ExecutionID,
		TurnID:      execution.TurnID,
		Status:      execution.State,
		CreatedAt:   execution.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:   execution.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	if execution.ResultEventID != "" {
		body.ResultEventID = &execution.ResultEventID
	}
	if execution.ErrorCode != "" {
		body.ErrorCode = &execution.ErrorCode
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		http.Error(w, "response encoding failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}
