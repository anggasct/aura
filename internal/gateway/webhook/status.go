package webhook

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const StatusPathPrefix = "/webhook/executions/"

type ExecutionStatus struct {
	ExecutionID   string
	TurnID        string
	State         string
	ResultEventID string
	ErrorCode     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	ExpiresAt     time.Time
}

type StatusStore interface {
	ExecutionStatus(ctx context.Context, id string) (ExecutionStatus, error)
}

type StatusHandler struct {
	keys      *KeyRing
	limiter   *RateLimiter
	clock     func() time.Time
	tolerance time.Duration
	logger    *slog.Logger
	status    StatusStore
}

func NewStatusHandler(keys *KeyRing, limiter *RateLimiter, clock func() time.Time, tolerance time.Duration, logger *slog.Logger, status StatusStore) (*StatusHandler, error) {
	if keys == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "key ring must not be nil")
	}
	if limiter == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "rate limiter must not be nil")
	}
	if clock == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "clock must not be nil")
	}
	if tolerance <= 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "timestamp tolerance must be positive")
	}
	if status == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "status store must not be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &StatusHandler{keys: keys, limiter: limiter, clock: clock, tolerance: tolerance, logger: logger, status: status}, nil
}

func (h *StatusHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, ErrorCodeInvalidRequest, "status endpoint accepts GET only")
		return
	}
	path := r.URL.EscapedPath()
	id, ok := strings.CutPrefix(path, StatusPathPrefix)
	if !ok || id == "" || strings.Contains(id, "/") {
		writeError(w, http.StatusNotFound, ErrorCodeExecutionNotFound, "unknown execution")
		return
	}
	auth, err := ParseRequestAuth(r.Header)
	if err != nil {
		writeErrorTyped(w, err)
		return
	}
	if allowed, _ := h.limiter.Allow(auth.KeyID, remoteAddr(r)); !allowed {
		writeError(w, http.StatusTooManyRequests, ErrorCodeRateLimited, "request budget exceeded")
		return
	}
	if !h.authenticate(auth, path) || !auth.WithinTolerance(h.clock(), h.tolerance) {
		writeError(w, http.StatusUnauthorized, ErrorCodeAuthFailed, "authentication failed")
		return
	}
	execution, err := h.status.ExecutionStatus(r.Context(), id)
	if err != nil {
		if code, ok := CodeOf(err); ok && code == ErrorCodeExecutionNotFound {
			writeError(w, http.StatusNotFound, ErrorCodeExecutionNotFound, "unknown execution")
			return
		}
		writeError(w, http.StatusInternalServerError, ErrorCodeInvalidRequest, "status is temporarily unavailable")
		return
	}
	if !execution.ExpiresAt.IsZero() && !h.clock().Before(execution.ExpiresAt) {
		writeError(w, http.StatusNotFound, ErrorCodeExecutionNotFound, "unknown execution")
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

func writeStatus(w http.ResponseWriter, execution *ExecutionStatus) {
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
