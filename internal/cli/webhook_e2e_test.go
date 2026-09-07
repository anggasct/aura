package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	gatewaywebhook "github.com/anggasct/aura/internal/gateway/webhook"
	"github.com/anggasct/aura/internal/store"
)

const webhookE2ESecret = "e2e-secret-0123456789abcdef"

func webhookE2EHandler(t *testing.T, backend *fakeTurnRuntime) (http.Handler, store.WebhookExecutionStore) {
	t.Helper()
	now := time.Unix(1750000000, 0).UTC()
	ring, err := gatewaywebhook.NewKeyRing(
		[]gatewaywebhook.KeyEntry{{ID: "primary", SecretEnv: "AURA_WEBHOOK_E2E_SECRET"}},
		func(envName string) (string, error) { return webhookE2ESecret, nil },
	)
	if err != nil {
		t.Fatalf("key ring construction failed: %v", err)
	}
	limiter, err := gatewaywebhook.NewRateLimiter(60, func() time.Time { return now })
	if err != nil {
		t.Fatalf("rate limiter construction failed: %v", err)
	}
	dispatcher, executions := webhookTestSetup(t, backend)
	handler, err := gatewaywebhook.NewHandler(gatewaywebhook.Settings{MaxBodySize: 4096, TimestampTolerance: 5 * time.Minute},
		ring, limiter, func() time.Time { return now }, slog.New(slog.DiscardHandler), dispatcher)
	if err != nil {
		t.Fatalf("handler construction failed: %v", err)
	}
	status, err := gatewaywebhook.NewStatusHandler(ring, limiter, func() time.Time { return now }, 5*time.Minute, slog.New(slog.DiscardHandler), &webhookStatusStore{executions: executions})
	if err != nil {
		t.Fatalf("status handler construction failed: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle(gatewaywebhook.StatusPathPrefix, status)
	mux.Handle("/", handler)
	return mux, executions
}

func webhookSignedPost(t *testing.T, body []byte, nonce string) *http.Request {
	t.Helper()
	timestamp := "1750000000"
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://webhook.test/webhook/event", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request construction failed: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Aura-Key-ID", "primary")
	req.Header.Set("X-Aura-Timestamp", timestamp)
	req.Header.Set("X-Aura-Nonce", nonce)
	req.Header.Set("X-Aura-Signature", gatewaywebhook.Sign(webhookE2ESecret, gatewaywebhook.EventSigningBytes(timestamp, nonce, body)))
	return req
}

func webhookSignedGet(t *testing.T, path, nonce string) *http.Request {
	t.Helper()
	timestamp := "1750000000"
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://webhook.test"+path, http.NoBody)
	if err != nil {
		t.Fatalf("request construction failed: %v", err)
	}
	req.Header.Set("X-Aura-Key-ID", "primary")
	req.Header.Set("X-Aura-Timestamp", timestamp)
	req.Header.Set("X-Aura-Nonce", nonce)
	req.Header.Set("X-Aura-Signature", gatewaywebhook.Sign(webhookE2ESecret, gatewaywebhook.StatusSigningBytes(timestamp, nonce, path)))
	return req
}

func TestBuildWebhookListener(t *testing.T) {
	t.Setenv("AURA_WEBHOOK_E2E_SECRET", webhookE2ESecret)
	ctx := context.Background()
	db, err := store.OpenDB(ctx, t.TempDir()+"/aura.db")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	backend := &fakeTurnRuntime{events: acceptedTurnEvents}
	cfg := &config.Config{
		Webhook: config.Webhook{
			Enabled:            true,
			Listen:             "127.0.0.1:0",
			MaxBodySize:        config.ByteSize(4096),
			TimestampTolerance: config.Duration(5 * time.Minute),
			ReplayRetention:    config.Duration(24 * time.Hour),
			RequestsPerMinute:  60,
			Keys:               []config.WebhookKey{{ID: "primary", SecretEnv: "AURA_WEBHOOK_E2E_SECRET"}},
		},
	}
	listener, err := buildWebhookListener(cfg, db, backend, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("buildWebhookListener: %v", err)
	}
	if listener.Name() != "webhook" {
		t.Errorf("listener name = %q, want webhook", listener.Name())
	}

	missing := *cfg
	missing.Webhook.Keys = []config.WebhookKey{{ID: "primary", SecretEnv: "AURA_WEBHOOK_E2E_MISSING"}}
	if _, err := buildWebhookListener(&missing, db, backend, slog.New(slog.DiscardHandler)); err == nil {
		t.Error("expected listener build to fail without a resolvable secret, got nil")
	}
}

func TestWebhookEndToEnd_AcceptThenStatus(t *testing.T) {
	backend := &fakeTurnRuntime{events: acceptedTurnEvents}
	mux, executions := webhookE2EHandler(t, backend)

	body := []byte(`{"event_id":"evt-1","subject":"hello","payload":{"k":1}}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, webhookSignedPost(t, body, "nonce-abcdefghijklmnop"))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	var accepted struct {
		ExecutionID string `json:"execution_id"`
		TurnID      string `json:"turn_id"`
		Status      string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("decode accept body: %v", err)
	}
	if accepted.ExecutionID == "" || accepted.TurnID == "" || accepted.Status != "accepted" {
		t.Fatalf("accept body mismatch: %+v", accepted)
	}
	if location := rec.Header().Get("Location"); location != "/webhook/executions/"+accepted.ExecutionID {
		t.Errorf("Location = %q, want the status path", location)
	}

	waitForExecutionState(t, executions, accepted.ExecutionID, store.WebhookExecutionStateCompleted)

	statusRec := httptest.NewRecorder()
	mux.ServeHTTP(statusRec, webhookSignedGet(t, "/webhook/executions/"+accepted.ExecutionID, "nonce-status-12345678"))
	if statusRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200: %s", statusRec.Code, statusRec.Body.String())
	}
	var status struct {
		ExecutionID   string  `json:"execution_id"`
		TurnID        string  `json:"turn_id"`
		Status        string  `json:"status"`
		ResultEventID *string `json:"result_event_id"`
		ErrorCode     *string `json:"error_code"`
	}
	if err := json.Unmarshal(statusRec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status body: %v", err)
	}
	if status.ExecutionID != accepted.ExecutionID || status.TurnID != accepted.TurnID || status.Status != "completed" {
		t.Errorf("status mismatch: %+v", status)
	}
	if status.ResultEventID == nil || *status.ResultEventID == "" {
		t.Error("expected a result event id on the completed status")
	}
	if status.ErrorCode != nil {
		t.Errorf("expected null error code, got %v", *status.ErrorCode)
	}
}

func TestWebhookEndToEnd_ReplayAndConflict(t *testing.T) {
	backend := &fakeTurnRuntime{events: acceptedTurnEvents}
	mux, _ := webhookE2EHandler(t, backend)

	body := []byte(`{"event_id":"evt-9","subject":"hello","payload":{"k":1}}`)
	first := httptest.NewRecorder()
	mux.ServeHTTP(first, webhookSignedPost(t, body, "nonce-replay-12345678"))
	if first.Code != http.StatusAccepted {
		t.Fatalf("first POST = %d, want 202", first.Code)
	}
	replay := httptest.NewRecorder()
	mux.ServeHTTP(replay, webhookSignedPost(t, body, "nonce-replay-12345678"))
	if replay.Code != http.StatusAccepted {
		t.Fatalf("replay POST = %d, want 202", replay.Code)
	}
	if first.Body.String() != replay.Body.String() {
		t.Errorf("replay body differs:\nfirst:  %s\nreplay: %s", first.Body.String(), replay.Body.String())
	}

	changed := []byte(`{"event_id":"evt-9","subject":"hello","payload":{"k":2}}`)
	conflict := httptest.NewRecorder()
	mux.ServeHTTP(conflict, webhookSignedPost(t, changed, "nonce-replay-12345678"))
	if conflict.Code != http.StatusConflict {
		t.Errorf("changed-body POST = %d, want 409: %s", conflict.Code, conflict.Body.String())
	}
	if backend.submitted() != 1 {
		t.Errorf("submitted turns = %d, want 1", backend.submitted())
	}
}
