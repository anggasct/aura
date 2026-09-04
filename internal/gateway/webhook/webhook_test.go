package webhook

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Vectors computed independently of the Go implementation (Python hmac over
// the documented canonical bytes) so the test proves the wire format, not
// just self-consistency.
const (
	vectorSecret    = "test-secret-0123456789abcdef"
	vectorTimestamp = "1750000000"
	vectorNonce     = "nonce-abcdefghijklmnop"
	vectorBody      = `{"event_id":"evt-1","subject":"hello","payload":{"k":1},"metadata":{"src":"ci"}}`
	vectorDigest    = "1b34c9c35e3ea80eb6c795e4e51cd9949fc087ac6085cf600024576ccaecdb16"
	vectorEventSig  = "af1a634da0640dda3797f86f1aae34a7b06d42f39f6220ce54f746cdbd216092"
	vectorStatusSig = "3640623f499cc75f269425c7ebb544e07741cd68ea15ee1802a2e2c5ad9ef3a0"
)

func TestEventSigningBytes_MatchesPublishedVector(t *testing.T) {
	got := EventSigningBytes(vectorTimestamp, vectorNonce, []byte(vectorBody))
	want := "v1\n" + vectorTimestamp + "\n" + vectorNonce + "\n" + vectorDigest
	if string(got) != want {
		t.Fatalf("event signing bytes mismatch:\n got %q\nwant %q", got, want)
	}
	if strings.HasSuffix(string(got), "\n") {
		t.Fatal("event signing bytes must not end with a newline")
	}
}

func TestStatusSigningBytes_MatchesPublishedVector(t *testing.T) {
	got := StatusSigningBytes(vectorTimestamp, vectorNonce, "/webhook/executions/01ABC")
	want := "v1\n" + vectorTimestamp + "\n" + vectorNonce + "\nGET\n/webhook/executions/01ABC"
	if string(got) != want {
		t.Fatalf("status signing bytes mismatch:\n got %q\nwant %q", got, want)
	}
	if Sign(vectorSecret, got) != "v1="+vectorStatusSig {
		t.Fatal("status signature does not match the published vector")
	}
}

func TestSign_MatchesPublishedVector(t *testing.T) {
	if got := Sign(vectorSecret, EventSigningBytes(vectorTimestamp, vectorNonce, []byte(vectorBody))); got != "v1="+vectorEventSig {
		t.Fatalf("signature mismatch: got %s", got)
	}
}

func TestVerifySignature(t *testing.T) {
	t.Run("accepts the published vector", func(t *testing.T) {
		if !VerifySignature("v1="+vectorEventSig, vectorSecret, EventSigningBytes(vectorTimestamp, vectorNonce, []byte(vectorBody))) {
			t.Fatal("valid signature rejected")
		}
	})
	t.Run("rejects uppercase hex", func(t *testing.T) {
		upper := "v1=" + strings.ToUpper(vectorEventSig)
		if VerifySignature(upper, vectorSecret, EventSigningBytes(vectorTimestamp, vectorNonce, []byte(vectorBody))) {
			t.Fatal("uppercase hex signature accepted")
		}
	})
	t.Run("rejects malformed headers", func(t *testing.T) {
		for _, header := range []string{"", "v1", "v2=" + vectorEventSig[3:], "v1=zz" + vectorEventSig[6:]} {
			if VerifySignature(header, vectorSecret, EventSigningBytes(vectorTimestamp, vectorNonce, []byte(vectorBody))) {
				t.Fatalf("malformed signature %q accepted", header)
			}
		}
	})
	t.Run("rejects wrong secret", func(t *testing.T) {
		if VerifySignature("v1="+vectorEventSig, "other-secret", EventSigningBytes(vectorTimestamp, vectorNonce, []byte(vectorBody))) {
			t.Fatal("signature verified under the wrong secret")
		}
	})
}

// recordingDispatcher captures the accepted event for assertions and
// controls the dispatch outcome.
type recordingDispatcher struct {
	mu     sync.Mutex
	events []AcceptedEvent
	result func(event *AcceptedEvent) (ExecutionRef, error)
}

func (d *recordingDispatcher) Dispatch(_ context.Context, event *AcceptedEvent) (ExecutionRef, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, *event)
	if d.result != nil {
		return d.result(event)
	}
	return ExecutionRef{ExecutionID: "01EXEC", TurnID: "01TURN"}, nil
}

type harness struct {
	handler  http.Handler
	dispatch *recordingDispatcher
	clock    func() time.Time
	logBuf   *bytes.Buffer
}

func fixedClock(at time.Time) func() time.Time { return func() time.Time { return at } }

func newHarness(t *testing.T, now time.Time, keys []KeyEntry, limit int) *harness {
	t.Helper()
	ring, err := NewKeyRing(keys, func(envName string) (string, error) {
		if value, ok := testKeySecrets[envName]; ok {
			return value, nil
		}
		return "", errors.New("secret unavailable")
	})
	if err != nil {
		t.Fatalf("key ring construction failed: %v", err)
	}
	limiter, err := NewRateLimiter(limit, fixedClock(now))
	if err != nil {
		t.Fatalf("rate limiter construction failed: %v", err)
	}
	dispatch := &recordingDispatcher{}
	logBuf := &bytes.Buffer{}
	handler, err := NewHandler(
		Settings{MaxBodySize: 1024, TimestampTolerance: 5 * time.Minute},
		ring, limiter, fixedClock(now),
		slog.New(slog.NewTextHandler(logBuf, nil)),
		dispatch,
	)
	if err != nil {
		t.Fatalf("handler construction failed: %v", err)
	}
	return &harness{handler: handler, dispatch: dispatch, clock: fixedClock(now), logBuf: logBuf}
}

var testKeySecrets = map[string]string{
	"AURA_TEST_PRIMARY": vectorSecret,
	"AURA_TEST_GRACE":   "grace-secret-9876543210",
	"AURA_TEST_EXPIRED": "expired-secret-1234567890",
}

func defaultTestKeys(now time.Time) []KeyEntry {
	return []KeyEntry{
		{ID: "primary", SecretEnv: "AURA_TEST_PRIMARY"},
		{ID: "grace", SecretEnv: "AURA_TEST_GRACE", AcceptUntil: now.Add(time.Hour)},
		{ID: "expired", SecretEnv: "AURA_TEST_EXPIRED", AcceptUntil: now.Add(-time.Hour)},
	}
}

func signedRequest(t *testing.T, method, path, keyID, secret, timestamp, nonce string, body []byte) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, "http://webhook.test"+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request construction failed: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerKeyID, keyID)
	req.Header.Set(headerTimestamp, timestamp)
	req.Header.Set(headerNonce, nonce)
	req.Header.Set(headerSignature, Sign(secret, EventSigningBytes(timestamp, nonce, body)))
	return req
}

func TestHandler_AcceptsValidRequest(t *testing.T) {
	now := time.Unix(1750000000, 0).UTC()
	h := newHarness(t, now, defaultTestKeys(now), 60)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, signedRequest(t, http.MethodPost, eventPath, "primary", vectorSecret, vectorTimestamp, vectorNonce, []byte(vectorBody)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var body acceptedBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response body not JSON: %v", err)
	}
	if body.ExecutionID != "01EXEC" || body.TurnID != "01TURN" || body.Status != "accepted" {
		t.Fatalf("unexpected response body: %+v", body)
	}
	if got := rec.Header().Get("Location"); got != "/webhook/executions/01EXEC" {
		t.Fatalf("Location = %q", got)
	}
	if len(h.dispatch.events) != 1 {
		t.Fatalf("dispatch called %d times", len(h.dispatch.events))
	}
	event := h.dispatch.events[0]
	if event.KeyID != "primary" || event.Nonce != vectorNonce || event.BodyDigest != vectorDigest || event.Envelope.EventID != "evt-1" {
		t.Fatalf("unexpected accepted event: %+v", event)
	}
}

func TestHandler_Routes(t *testing.T) {
	now := time.Unix(1750000000, 0).UTC()
	h := newHarness(t, now, defaultTestKeys(now), 60)
	t.Run("unknown path is 404", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, signedRequest(t, http.MethodPost, "/other", "primary", vectorSecret, vectorTimestamp, vectorNonce, []byte(vectorBody)))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d", rec.Code)
		}
	})
	t.Run("GET on the event route is 405", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, signedRequest(t, http.MethodGet, eventPath, "primary", vectorSecret, vectorTimestamp, vectorNonce, []byte(vectorBody)))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d", rec.Code)
		}
	})
	t.Run("non-JSON content type is 415", func(t *testing.T) {
		req := signedRequest(t, http.MethodPost, eventPath, "primary", vectorSecret, vectorTimestamp, vectorNonce, []byte(vectorBody))
		req.Header.Set("Content-Type", "text/plain")
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		assertErrorBody(t, rec, http.StatusUnsupportedMediaType, string(ErrorCodeMediaUnsupported))
	})
}

func TestHandler_TamperingRejectedBeforeDispatch(t *testing.T) {
	now := time.Unix(1750000000, 0).UTC()
	readableBody := func(body []byte) io.ReadCloser { return io.NopCloser(bytes.NewReader(body)) }
	base := func(mutate func(req *http.Request, body []byte) []byte) (*httptest.ResponseRecorder, *harness) {
		h := newHarness(t, now, defaultTestKeys(now), 60)
		body := []byte(vectorBody)
		req := signedRequest(t, http.MethodPost, eventPath, "primary", vectorSecret, vectorTimestamp, vectorNonce, body)
		body = mutate(req, body)
		req.Body = readableBody(body)
		req.ContentLength = int64(len(body))
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		return rec, h
	}

	cases := []struct {
		name   string
		mutate func(req *http.Request, body []byte) []byte
		status int
		code   string
	}{
		{
			name: "body bytes changed after signing",
			mutate: func(req *http.Request, body []byte) []byte {
				return []byte(strings.Replace(vectorBody, `"k":1`, `"k":2`, 1))
			},
			status: http.StatusUnauthorized, code: string(ErrorCodeAuthFailed),
		},
		{
			name: "timestamp header changed after signing",
			mutate: func(req *http.Request, body []byte) []byte {
				req.Header.Set(headerTimestamp, "1750000060")
				return body
			},
			status: http.StatusUnauthorized, code: string(ErrorCodeAuthFailed),
		},
		{
			name: "nonce header changed after signing",
			mutate: func(req *http.Request, body []byte) []byte {
				req.Header.Set(headerNonce, "nonce-ZZZZZZZZZZZZZZZZ")
				return body
			},
			status: http.StatusUnauthorized, code: string(ErrorCodeAuthFailed),
		},
		{
			name: "signature header truncated",
			mutate: func(req *http.Request, body []byte) []byte {
				value := req.Header.Get(headerSignature)
				req.Header.Set(headerSignature, value[:len(value)-1])
				return body
			},
			status: http.StatusBadRequest, code: string(ErrorCodeInvalidRequest),
		},
		{
			name: "unknown key id",
			mutate: func(req *http.Request, body []byte) []byte {
				req.Header.Set(headerKeyID, "ghost")
				return body
			},
			status: http.StatusUnauthorized, code: string(ErrorCodeAuthFailed),
		},
		{
			name: "expired key id",
			mutate: func(req *http.Request, body []byte) []byte {
				req.Header.Set(headerKeyID, "expired")
				req.Header.Set(headerSignature, Sign("expired-secret-1234567890", EventSigningBytes(vectorTimestamp, vectorNonce, body)))
				return body
			},
			status: http.StatusUnauthorized, code: string(ErrorCodeAuthFailed),
		},
		{
			name: "timestamp outside tolerance",
			mutate: func(req *http.Request, body []byte) []byte {
				stale := "1749999000"
				req.Header.Set(headerTimestamp, stale)
				req.Header.Set(headerSignature, Sign(vectorSecret, EventSigningBytes(stale, vectorNonce, body)))
				return body
			},
			status: http.StatusUnauthorized, code: string(ErrorCodeAuthFailed),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, h := base(tc.mutate)
			assertErrorBody(t, rec, tc.status, tc.code)
			if len(h.dispatch.events) != 0 {
				t.Fatalf("tampered request was dispatched: %+v", h.dispatch.events)
			}
		})
	}
}

func TestHandler_GraceKeyStillVerifies(t *testing.T) {
	now := time.Unix(1750000000, 0).UTC()
	h := newHarness(t, now, defaultTestKeys(now), 60)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, signedRequest(t, http.MethodPost, eventPath, "grace", "grace-secret-9876543210", vectorTimestamp, vectorNonce, []byte(vectorBody)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("grace-period key rejected: status %d, body %s", rec.Code, rec.Body.String())
	}
}

func TestHandler_RateLimit(t *testing.T) {
	current := time.Unix(1750000000, 0).UTC()
	keys := defaultTestKeys(current)
	ring, err := NewKeyRing(keys, func(envName string) (string, error) {
		return testKeySecrets[envName], nil
	})
	if err != nil {
		t.Fatalf("key ring construction failed: %v", err)
	}
	limiter, err := NewRateLimiter(3, func() time.Time { return current })
	if err != nil {
		t.Fatalf("rate limiter construction failed: %v", err)
	}
	dispatch := &recordingDispatcher{}
	handler, err := NewHandler(Settings{MaxBodySize: 1024, TimestampTolerance: 5 * time.Minute}, ring, limiter, func() time.Time { return current }, slog.New(slog.DiscardHandler), dispatch)
	if err != nil {
		t.Fatalf("handler construction failed: %v", err)
	}
	secondKeyBody := []byte(`{"event_id":"evt-2","subject":"s","payload":{}}`)
	nonce := func(n int) string { return vectorNonce[:10] + strings.Repeat("x", 4) + string(rune('a'+n)) + "000000" }
	for i := range 3 {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, signedRequest(t, http.MethodPost, eventPath, "primary", vectorSecret, vectorTimestamp, nonce(i), []byte(vectorBody)))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("request %d rejected: %d", i, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, signedRequest(t, http.MethodPost, eventPath, "primary", vectorSecret, vectorTimestamp, nonce(3), []byte(vectorBody)))
	assertErrorBody(t, rec, http.StatusTooManyRequests, string(ErrorCodeRateLimited))
	if len(dispatch.events) != 3 {
		t.Fatalf("dispatch called %d times", len(dispatch.events))
	}
	// A different key from a different address is an independent budget.
	rec = httptest.NewRecorder()
	graceReq := signedRequest(t, http.MethodPost, eventPath, "grace", "grace-secret-9876543210", vectorTimestamp, nonce(4), secondKeyBody)
	graceReq.RemoteAddr = "10.0.0.2:5555"
	handler.ServeHTTP(rec, graceReq)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("independent key budget not respected: %d", rec.Code)
	}
	// The window resets after a minute.
	current = current.Add(61 * time.Second)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, signedRequest(t, http.MethodPost, eventPath, "primary", vectorSecret, "1750000061", nonce(5), []byte(vectorBody)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("window did not reset: %d", rec.Code)
	}
}

func TestHandler_BodyTooLarge(t *testing.T) {
	now := time.Unix(1750000000, 0).UTC()
	h := newHarness(t, now, defaultTestKeys(now), 60)
	huge := append([]byte(`{"event_id":"e","subject":"s","payload":{"pad":"`), bytes.Repeat([]byte("a"), 4096)...)
	huge = append(huge, []byte(`"}`)...)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, signedRequest(t, http.MethodPost, eventPath, "primary", vectorSecret, vectorTimestamp, vectorNonce, huge))
	assertErrorBody(t, rec, http.StatusRequestEntityTooLarge, string(ErrorCodeBodyTooLarge))
	if len(h.dispatch.events) != 0 {
		t.Fatal("oversized request was dispatched")
	}
}

func TestHandler_LogsContainNoSecrets(t *testing.T) {
	now := time.Unix(1750000000, 0).UTC()
	h := newHarness(t, now, defaultTestKeys(now), 60)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, signedRequest(t, http.MethodPost, eventPath, "primary", vectorSecret, vectorTimestamp, vectorNonce, []byte(vectorBody)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}
	logs := h.logBuf.String()
	for _, canary := range []string{vectorSecret, "grace-secret-9876543210", "expired-secret-1234567890", "v1=", vectorEventSig, vectorNonce, `"k":1`, "hello"} {
		if strings.Contains(logs, canary) {
			t.Fatalf("log output leaks %q:\n%s", canary, logs)
		}
	}
	for _, want := range []string{"key_id=primary", "body_digest=" + vectorDigest, "status=accepted", "latency_ms="} {
		if !strings.Contains(logs, want) {
			t.Fatalf("log output missing %q:\n%s", want, logs)
		}
	}
}

func TestParseEnvelope(t *testing.T) {
	t.Run("accepts the reference body", func(t *testing.T) {
		envelope, err := ParseEnvelope([]byte(vectorBody))
		if err != nil {
			t.Fatalf("valid body rejected: %v", err)
		}
		if envelope.EventID != "evt-1" || envelope.Subject != "hello" || envelope.Metadata["src"] != "ci" {
			t.Fatalf("unexpected envelope: %+v", envelope)
		}
	})
	cases := []struct {
		name string
		body string
	}{
		{"unknown top-level field", `{"event_id":"e","subject":"s","payload":{},"extra":1}`},
		{"missing event_id", `{"subject":"s","payload":{}}`},
		{"empty event_id", `{"event_id":"","subject":"s","payload":{}}`},
		{"event_id with control character", "{\"event_id\":\"a\tb\",\"subject\":\"s\",\"payload\":{}}"},
		{"event_id too long", `{"event_id":"` + strings.Repeat("x", 201) + `","subject":"s","payload":{}}`},
		{"subject too long", `{"event_id":"e","subject":"` + strings.Repeat("x", 501) + `","payload":{}}`},
		{"payload not an object", `{"event_id":"e","subject":"s","payload":[1,2]}`},
		{"payload missing", `{"event_id":"e","subject":"s"}`},
		{"metadata non-string value", `{"event_id":"e","subject":"s","payload":{},"metadata":{"n":1}}`},
		{"trailing content", vectorBody + ` {}`},
		{"not JSON", `not json at all`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseEnvelope([]byte(tc.body)); err == nil {
				t.Fatal("invalid body accepted")
			} else if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidRequest {
				t.Fatalf("error code = %v, want webhook_invalid_request", code)
			}
		})
	}
	t.Run("metadata over 32 entries", func(t *testing.T) {
		var b strings.Builder
		b.WriteString(`{"event_id":"e","subject":"s","payload":{},"metadata":{`)
		for i := range 33 {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`"k` + string(rune('a'+i%26)) + string(rune('a'+i/26)) + `":"v"`)
		}
		b.WriteString(`}}`)
		if _, err := ParseEnvelope([]byte(b.String())); err == nil {
			t.Fatal("oversized metadata accepted")
		}
	})
}

func TestParseRequestAuth_RejectsMalformed(t *testing.T) {
	valid := http.Header{}
	valid.Set(headerKeyID, "primary")
	valid.Set(headerTimestamp, vectorTimestamp)
	valid.Set(headerNonce, vectorNonce)
	valid.Set(headerSignature, "v1="+vectorEventSig)
	if _, err := ParseRequestAuth(valid); err != nil {
		t.Fatalf("valid headers rejected: %v", err)
	}
	cases := map[string]func(http.Header){
		"missing key id":         func(h http.Header) { h.Del(headerKeyID) },
		"key id bad charset":     func(h http.Header) { h.Set(headerKeyID, "key id!") },
		"timestamp not decimal":  func(h http.Header) { h.Set(headerTimestamp, "not-a-number") },
		"timestamp float":        func(h http.Header) { h.Set(headerTimestamp, "1750000000.5") },
		"nonce too short":        func(h http.Header) { h.Set(headerNonce, "short") },
		"nonce bad charset":      func(h http.Header) { h.Set(headerNonce, "nonce with space!!") },
		"signature wrong prefix": func(h http.Header) { h.Set(headerSignature, "v2="+vectorEventSig) },
		"signature uppercase":    func(h http.Header) { h.Set(headerSignature, "v1="+strings.ToUpper(vectorEventSig)) },
		"signature short":        func(h http.Header) { h.Set(headerSignature, "v1=abcd") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			header := valid.Clone()
			mutate(header)
			if _, err := ParseRequestAuth(header); err == nil {
				t.Fatal("malformed headers accepted")
			}
		})
	}
}

func TestKeyRing(t *testing.T) {
	t.Run("duplicate key ids are rejected", func(t *testing.T) {
		entries := []KeyEntry{{ID: "a", SecretEnv: "X"}, {ID: "a", SecretEnv: "Y"}}
		if _, err := NewKeyRing(entries, func(string) (string, error) { return "s", nil }); err == nil {
			t.Fatal("duplicate key ids accepted")
		}
	})
	t.Run("unresolvable secret fails closed", func(t *testing.T) {
		entries := []KeyEntry{{ID: "a", SecretEnv: "MISSING"}}
		_, err := NewKeyRing(entries, func(string) (string, error) { return "", errors.New("gone") })
		if code, ok := CodeOf(err); !ok || code != ErrorCodeKeyResolutionFailed {
			t.Fatalf("error code = %v, want webhook_key_resolution_failed", code)
		}
	})
	t.Run("nil entries are rejected", func(t *testing.T) {
		if _, err := NewKeyRing(nil, func(string) (string, error) { return "s", nil }); err == nil {
			t.Fatal("nil entries accepted")
		}
	})
}

func TestNewHandler_NilGuards(t *testing.T) {
	now := fixedClock(time.Unix(1750000000, 0).UTC())
	ring, err := NewKeyRing([]KeyEntry{{ID: "a", SecretEnv: "X"}}, func(string) (string, error) { return "s", nil })
	if err != nil {
		t.Fatalf("key ring construction failed: %v", err)
	}
	limiter, err := NewRateLimiter(1, now)
	if err != nil {
		t.Fatalf("rate limiter construction failed: %v", err)
	}
	dispatch := &recordingDispatcher{}
	settings := Settings{MaxBodySize: 128, TimestampTolerance: time.Minute}
	if _, err := NewHandler(settings, nil, limiter, now, nil, dispatch); err == nil {
		t.Fatal("nil key ring accepted")
	}
	if _, err := NewHandler(settings, ring, nil, now, nil, dispatch); err == nil {
		t.Fatal("nil rate limiter accepted")
	}
	if _, err := NewHandler(settings, ring, limiter, nil, nil, dispatch); err == nil {
		t.Fatal("nil clock accepted")
	}
	if _, err := NewHandler(settings, ring, limiter, now, nil, nil); err == nil {
		t.Fatal("nil dispatcher accepted")
	}
	if _, err := NewHandler(Settings{}, ring, limiter, now, nil, dispatch); err == nil {
		t.Fatal("zero settings accepted")
	}
}

func assertErrorBody(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d, body %s", rec.Code, wantStatus, rec.Body.String())
	}
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body not JSON: %v", err)
	}
	if body.Error.Code != wantCode || body.Error.Message == "" {
		t.Fatalf("unexpected error body: %+v", body)
	}
}

func TestDigestIsLowercaseHex(t *testing.T) {
	body := []byte(vectorBody)
	sum := sha256.Sum256(body)
	got := hex.EncodeToString(sum[:])
	if got != vectorDigest {
		t.Fatalf("digest = %s, want %s", got, vectorDigest)
	}
}
