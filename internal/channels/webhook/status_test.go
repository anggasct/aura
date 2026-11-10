package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type stubStatusStore struct {
	status ExecutionStatus
	err    error
}

func (s *stubStatusStore) ExecutionStatus(_ context.Context, _ string) (ExecutionStatus, error) {
	return s.status, s.err
}

func statusFixture(now time.Time) ExecutionStatus {
	return ExecutionStatus{
		ExecutionID: "whex_1",
		TurnID:      "turn_1",
		State:       "completed",
		CreatedAt:   now.Add(-time.Hour),
		UpdatedAt:   now,
		ExpiresAt:   now.Add(23 * time.Hour),
	}
}

func newStatusHarness(t *testing.T, now time.Time, store StatusStore) (*StatusHandler, *bytes.Buffer) {
	t.Helper()
	ring, err := NewKeyRing([]KeyEntry{{ID: "primary", SecretEnv: "AURA_TEST_PRIMARY"}}, func(envName string) (string, error) {
		if value, ok := testKeySecrets[envName]; ok {
			return value, nil
		}
		return "", errors.New("secret unavailable")
	})
	if err != nil {
		t.Fatalf("key ring construction failed: %v", err)
	}
	limiter, err := NewRateLimiter(60, fixedClock(now))
	if err != nil {
		t.Fatalf("rate limiter construction failed: %v", err)
	}
	logBuf := &bytes.Buffer{}
	handler, err := NewStatusHandler(ring, limiter, fixedClock(now), 5*time.Minute, slog.New(slog.NewTextHandler(logBuf, nil)), store)
	if err != nil {
		t.Fatalf("status handler construction failed: %v", err)
	}
	return handler, logBuf
}

func signedStatusRequest(t *testing.T, path, timestamp, nonce, signature string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://webhook.test"+path, http.NoBody)
	if err != nil {
		t.Fatalf("request construction failed: %v", err)
	}
	req.Header.Set(headerKeyID, "primary")
	req.Header.Set(headerTimestamp, timestamp)
	req.Header.Set(headerNonce, nonce)
	req.Header.Set(headerSignature, signature)
	return req
}

func TestStatusHandler_ServesCompletedExecution(t *testing.T) {
	now := time.Unix(1750000000, 0).UTC()
	handler, _ := newStatusHarness(t, now, &stubStatusStore{status: statusFixture(now)})

	path := "/webhook/executions/whex_1"
	sig := Sign(vectorSecret, StatusSigningBytes(vectorTimestamp, vectorNonce, path))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, signedStatusRequest(t, path, vectorTimestamp, vectorNonce, sig))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body statusBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode status body: %v", err)
	}
	if body.ExecutionID != "whex_1" || body.TurnID != "turn_1" || body.Status != "completed" {
		t.Errorf("identity mismatch: %+v", body)
	}
	if body.ResultEventID != nil || body.ErrorCode != nil {
		t.Errorf("expected null result fields, got %+v", body)
	}
}

func TestStatusHandler_ServesFailedExecution(t *testing.T) {
	now := time.Unix(1750000000, 0).UTC()
	fixture := statusFixture(now)
	fixture.State = "failed"
	fixture.ResultEventID = "evt_f1"
	fixture.ErrorCode = "runtime_overloaded"
	handler, _ := newStatusHarness(t, now, &stubStatusStore{status: fixture})

	path := "/webhook/executions/whex_1"
	sig := Sign(vectorSecret, StatusSigningBytes(vectorTimestamp, vectorNonce, path))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, signedStatusRequest(t, path, vectorTimestamp, vectorNonce, sig))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body statusBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode status body: %v", err)
	}
	if body.ResultEventID == nil || *body.ResultEventID != "evt_f1" {
		t.Errorf("result event id = %v, want evt_f1", body.ResultEventID)
	}
	if body.ErrorCode == nil || *body.ErrorCode != "runtime_overloaded" {
		t.Errorf("error code = %v, want runtime_overloaded", body.ErrorCode)
	}
}

func TestStatusHandler_RejectsBadAuthentication(t *testing.T) {
	now := time.Unix(1750000000, 0).UTC()
	handler, logBuf := newStatusHarness(t, now, &stubStatusStore{status: statusFixture(now)})
	path := "/webhook/executions/whex_1"

	cases := []struct {
		name      string
		timestamp string
		nonce     string
		signature string
		want      int
	}{
		{"tampered signature", vectorTimestamp, vectorNonce, "v1=" + strings.Repeat("0", 64), http.StatusUnauthorized},
		{"event signature on status path", vectorTimestamp, vectorNonce, "v1=" + vectorEventSig, http.StatusUnauthorized},
		{"stale timestamp", "1700000000", vectorNonce, "", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sig := tc.signature
			if tc.name == "stale timestamp" {
				sig = Sign(vectorSecret, StatusSigningBytes(tc.timestamp, tc.nonce, path))
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, signedStatusRequest(t, path, tc.timestamp, tc.nonce, sig))
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
	logged := logBuf.String()
	for _, secret := range []string{vectorSecret, vectorNonce, "v1="} {
		if strings.Contains(logged, secret) {
			t.Errorf("status log contains sensitive material %q", secret)
		}
	}
}

func TestStatusHandler_UnknownAndExpiredAreNotFound(t *testing.T) {
	now := time.Unix(1750000000, 0).UTC()
	path := "/webhook/executions/whex_missing"
	sig := Sign(vectorSecret, StatusSigningBytes(vectorTimestamp, vectorNonce, path))

	unknown, _ := newStatusHarness(t, now, &stubStatusStore{err: Errorf(ErrorCodeExecutionNotFound, "unknown execution")})
	rec := httptest.NewRecorder()
	unknown.ServeHTTP(rec, signedStatusRequest(t, path, vectorTimestamp, vectorNonce, sig))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown execution status = %d, want 404", rec.Code)
	}

	expiredFixture := statusFixture(now)
	expiredFixture.ExpiresAt = now.Add(-time.Minute)
	expired, _ := newStatusHarness(t, now, &stubStatusStore{status: expiredFixture})
	rec = httptest.NewRecorder()
	expired.ServeHTTP(rec, signedStatusRequest(t, path, vectorTimestamp, vectorNonce, sig))
	if rec.Code != http.StatusNotFound {
		t.Errorf("expired execution status = %d, want 404", rec.Code)
	}
}

func TestStatusHandler_RejectsWrongMethodAndPath(t *testing.T) {
	now := time.Unix(1750000000, 0).UTC()
	handler, _ := newStatusHarness(t, now, &stubStatusStore{status: statusFixture(now)})

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://webhook.test/webhook/executions/whex_1", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", rec.Code)
	}

	bare, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://webhook.test/webhook/executions/", http.NoBody)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, bare)
	if rec.Code != http.StatusNotFound {
		t.Errorf("bare prefix status = %d, want 404", rec.Code)
	}
}
