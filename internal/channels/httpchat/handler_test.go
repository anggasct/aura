package httpchat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/anggasct/aura/internal/platform/principal"
)

const (
	testWait      = 30 * time.Second
	testBodyLimit = 64
)

var testCreated = time.Date(2026, 11, 5, 9, 30, 0, 0, time.UTC)

type memoryKeys struct {
	mu      sync.Mutex
	keys    map[string]Key
	lookups atomic.Int64
	failure error
}

func (m *memoryKeys) GetByID(_ context.Context, id string) (Key, bool, error) {
	m.lookups.Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failure != nil {
		return Key{}, false, m.failure
	}
	key, ok := m.keys[id]
	return key, ok, nil
}

func (m *memoryKeys) add(t *testing.T, class principal.Class, persona string) IssuedKey {
	t.Helper()
	issued, err := NewKey()
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.keys == nil {
		m.keys = map[string]Key{}
	}
	m.keys[issued.ID] = Key{ID: issued.ID, Digest: issued.Digest, PersonaID: persona, Class: class, CreatedAt: testCreated}
	return issued
}

func (m *memoryKeys) revoke(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := m.keys[id]
	key.Revoked = true
	m.keys[id] = key
}

type recordingChat struct {
	mu    sync.Mutex
	calls []Call
	enter chan struct{}
	hold  chan struct{}
}

func (c *recordingChat) ServeChat(w http.ResponseWriter, _ *http.Request, call *Call) {
	c.mu.Lock()
	c.calls = append(c.calls, *call)
	c.mu.Unlock()
	if c.enter != nil {
		c.enter <- struct{}{}
	}
	if c.hold != nil {
		<-c.hold
	}
	w.WriteHeader(http.StatusNoContent)
}

func (c *recordingChat) received() []Call {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Call(nil), c.calls...)
}

type handlerFixture struct {
	handler *Handler
	keys    *memoryKeys
	chat    *recordingChat
	logs    *bytes.Buffer
}

type fixtureOptions struct {
	rate        int
	concurrency int
	chat        *recordingChat
	noChat      bool
}

func newFixture(t *testing.T, opts fixtureOptions) *handlerFixture {
	t.Helper()
	logs := &bytes.Buffer{}
	fixture := &handlerFixture{keys: &memoryKeys{}, chat: opts.chat, logs: logs}
	if fixture.chat == nil {
		fixture.chat = &recordingChat{}
	}
	options := &Options{
		Keys:                fixture.keys,
		MaxBodyBytes:        testBodyLimit,
		RequestsPerMinute:   cmpOr(opts.rate, 600),
		MaxConcurrentPerKey: cmpOr(opts.concurrency, 4),
		Logger:              slog.New(slog.NewTextHandler(&lockedWriter{w: logs}, nil)),
	}
	if !opts.noChat {
		options.Chat = fixture.chat
	}
	handler, err := NewHandler(options)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	fixture.handler = handler
	return fixture
}

func cmpOr(value, fallback int) int {
	if value != 0 {
		return value
	}
	return fallback
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func (f *handlerFixture) do(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func request(t *testing.T, method, path, bearer string, body io.Reader) *http.Request {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, body)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return req
}

func jsonPost(t *testing.T, bearer, body string) *http.Request {
	t.Helper()
	req := request(t, http.MethodPost, routeChatCompletions, bearer, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

type envelope struct {
	Error struct {
		Message string  `json:"message"`
		Type    string  `json:"type"`
		Param   *string `json:"param"`
		Code    string  `json:"code"`
	} `json:"error"`
}

func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) envelope {
	t.Helper()
	var decoded envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode error envelope %q: %v", rec.Body.String(), err)
	}
	return decoded
}

func expectError(t *testing.T, rec *httptest.ResponseRecorder, status int, kind, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, status, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("content type = %q, want application/json", got)
	}
	decoded := decodeEnvelope(t, rec)
	if decoded.Error.Type != kind || decoded.Error.Code != code || decoded.Error.Message == "" || decoded.Error.Param != nil {
		t.Fatalf("envelope = %+v, want type %s code %s with a message and null param", decoded.Error, kind, code)
	}
}

func TestHandlerRoutesAndMethodsAnswerBeforeAnyKeyLookup(t *testing.T) {
	cases := map[string]struct {
		method, path string
		noChat       bool
		status       int
		kind, code   string
		allow        string
	}{
		"unknown route":             {method: http.MethodGet, path: "/v1/unknown", status: 404, kind: "invalid_request_error", code: "not_found"},
		"root":                      {method: http.MethodGet, path: "/", status: 404, kind: "invalid_request_error", code: "not_found"},
		"trailing slash":            {method: http.MethodGet, path: "/v1/models/", status: 404, kind: "invalid_request_error", code: "not_found"},
		"case differs":              {method: http.MethodGet, path: "/V1/models", status: 404, kind: "invalid_request_error", code: "not_found"},
		"chat route is not mounted": {method: http.MethodPost, path: routeChatCompletions, noChat: true, status: 404, kind: "invalid_request_error", code: "not_found"},
		"models rejects post":       {method: http.MethodPost, path: routeModels, status: 405, kind: "invalid_request_error", code: "method_not_allowed", allow: "GET"},
		"models rejects delete":     {method: http.MethodDelete, path: routeModels, status: 405, kind: "invalid_request_error", code: "method_not_allowed", allow: "GET"},
		"models rejects head":       {method: http.MethodHead, path: routeModels, status: 405, kind: "invalid_request_error", code: "method_not_allowed", allow: "GET"},
		"chat rejects get":          {method: http.MethodGet, path: routeChatCompletions, status: 405, kind: "invalid_request_error", code: "method_not_allowed", allow: "POST"},
		"chat rejects put":          {method: http.MethodPut, path: routeChatCompletions, status: 405, kind: "invalid_request_error", code: "method_not_allowed", allow: "POST"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newFixture(t, fixtureOptions{noChat: tc.noChat})
			issued := fixture.keys.add(t, principal.ClassTrusted, "main")
			rec := fixture.do(request(t, tc.method, tc.path, issued.Plaintext, nil))
			expectError(t, rec, tc.status, tc.kind, tc.code)
			if got := rec.Header().Get("Allow"); got != tc.allow {
				t.Errorf("Allow = %q, want %q", got, tc.allow)
			}
			if lookups := fixture.keys.lookups.Load(); lookups != 0 {
				t.Errorf("key store was consulted %d times before route and method checks", lookups)
			}
		})
	}
}

func TestHandlerAuthenticationFailuresAreIndistinguishable(t *testing.T) {
	fixture := newFixture(t, fixtureOptions{})
	valid := fixture.keys.add(t, principal.ClassTrusted, "main")
	revoked := fixture.keys.add(t, principal.ClassTrusted, "main")
	fixture.keys.revoke(revoked.ID)
	unknown, err := NewKey()
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	wrongSecret := valid.Plaintext[:len(valid.Plaintext)-1] + "z"
	withHeaders := func(values ...string) *http.Request {
		req := request(t, http.MethodGet, routeModels, "", nil)
		req.Header["Authorization"] = values
		return req
	}
	cases := map[string]struct {
		req     *http.Request
		lookups int64
	}{
		"missing header":         {req: request(t, http.MethodGet, routeModels, "", nil)},
		"empty bearer":           {req: withHeaders("Bearer ")},
		"bearer without token":   {req: withHeaders("Bearer")},
		"basic scheme":           {req: withHeaders("Basic " + valid.Plaintext)},
		"raw key without scheme": {req: withHeaders(valid.Plaintext)},
		"duplicate headers":      {req: withHeaders("Bearer "+valid.Plaintext, "Bearer "+valid.Plaintext)},
		"malformed token":        {req: request(t, http.MethodGet, routeModels, "aura_sk_short", nil)},
		"foreign token":          {req: request(t, http.MethodGet, routeModels, "sk-proj-abcdef", nil)},
		"unknown key":            {req: request(t, http.MethodGet, routeModels, unknown.Plaintext, nil), lookups: 1},
		"wrong secret":           {req: request(t, http.MethodGet, routeModels, wrongSecret, nil), lookups: 1},
		"revoked key":            {req: request(t, http.MethodGet, routeModels, revoked.Plaintext, nil), lookups: 1},
	}
	var reference []byte
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			before := fixture.keys.lookups.Load()
			rec := fixture.do(tc.req)
			expectError(t, rec, 401, "authentication_error", "invalid_api_key")
			if got := fixture.keys.lookups.Load() - before; got != tc.lookups {
				t.Errorf("key lookups = %d, want %d", got, tc.lookups)
			}
			if reference == nil {
				reference = rec.Body.Bytes()
			} else if !bytes.Equal(reference, rec.Body.Bytes()) {
				t.Errorf("body %q differs from %q", rec.Body.String(), reference)
			}
			if rec.Header().Get("WWW-Authenticate") != "" || rec.Header().Get("Retry-After") != "" {
				t.Errorf("headers leak a distinguishing hint: %v", rec.Header())
			}
		})
	}
}

func TestHandlerAuthenticationRunsBeforeContentTypeAndBodyChecks(t *testing.T) {
	fixture := newFixture(t, fixtureOptions{})
	bad := httptest.NewRequestWithContext(t.Context(), http.MethodPost, routeChatCompletions, strings.NewReader(strings.Repeat("x", testBodyLimit*4)))
	bad.Header.Set("Content-Type", "text/plain")
	expectError(t, fixture.do(bad), 401, "authentication_error", "invalid_api_key")
	if calls := fixture.chat.received(); len(calls) != 0 {
		t.Fatalf("chat handler ran %d times for an unauthenticated request", len(calls))
	}
}

func TestHandlerStoreFailureDeniesWithInternalError(t *testing.T) {
	fixture := newFixture(t, fixtureOptions{})
	issued := fixture.keys.add(t, principal.ClassOwner, "main")
	fixture.keys.failure = errors.New("disk detail that must stay private")
	rec := fixture.do(request(t, http.MethodGet, routeModels, issued.Plaintext, nil))
	expectError(t, rec, 500, "server_error", "internal_error")
	if strings.Contains(rec.Body.String(), "disk detail") {
		t.Errorf("body leaks the store error: %s", rec.Body.String())
	}
	if len(fixture.chat.received()) != 0 {
		t.Error("chat handler ran after a store failure")
	}
}

func TestHandlerModelsListsExactlyTheKeysPersona(t *testing.T) {
	fixture := newFixture(t, fixtureOptions{})
	fixture.keys.add(t, principal.ClassTrusted, "other")
	issued := fixture.keys.add(t, principal.ClassPublic, "kantor")
	rec := fixture.do(request(t, http.MethodGet, routeModels, issued.Plaintext, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("content type = %q", got)
	}
	want := `{"object":"list","data":[{"id":"kantor","object":"model","created":` + strconv.FormatInt(testCreated.Unix(), 10) + `,"owned_by":"aura"}]}`
	if rec.Body.String() != want {
		t.Fatalf("body = %s, want %s", rec.Body.String(), want)
	}
}

func TestHandlerPrincipalResolution(t *testing.T) {
	cases := map[string]struct {
		class  principal.Class
		wantID func(keyID string) string
	}{
		"trusted key": {class: principal.ClassTrusted, wantID: func(id string) string { return "http:" + id }},
		"public key":  {class: principal.ClassPublic, wantID: func(id string) string { return "http:" + id }},
		"owner key":   {class: principal.ClassOwner, wantID: func(string) string { return principal.OwnerID }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newFixture(t, fixtureOptions{})
			issued := fixture.keys.add(t, tc.class, "kantor")
			rec := fixture.do(jsonPost(t, issued.Plaintext, `{"raw":`))
			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
			}
			calls := fixture.chat.received()
			if len(calls) != 1 {
				t.Fatalf("chat calls = %d, want 1", len(calls))
			}
			got := calls[0].Principal
			if got.ID != tc.wantID(issued.ID) || got.Class != tc.class || got.PersonaID != "kantor" || got.KeyID != issued.ID {
				t.Fatalf("principal = %+v", got)
			}
			if string(calls[0].Body) != `{"raw":` {
				t.Fatalf("body was altered before the chat handler: %q", calls[0].Body)
			}
		})
	}
}

func TestHandlerContentTypeAndBodyBound(t *testing.T) {
	exact := strings.Repeat("a", testBodyLimit)
	cases := map[string]struct {
		contentType string
		body        io.Reader
		length      int64
		status      int
		code        string
	}{
		"missing content type":      {body: strings.NewReader("{}"), status: 415, code: "unsupported_media_type"},
		"text plain":                {contentType: "text/plain", body: strings.NewReader("{}"), status: 415, code: "unsupported_media_type"},
		"form encoded":              {contentType: "application/x-www-form-urlencoded", body: strings.NewReader("a=b"), status: 415, code: "unsupported_media_type"},
		"unparsable content type":   {contentType: "application/json;;;=", body: strings.NewReader("{}"), status: 415, code: "unsupported_media_type"},
		"json with charset":         {contentType: "application/json; charset=utf-8", body: strings.NewReader("{}"), status: 204},
		"json upper case":           {contentType: "Application/JSON", body: strings.NewReader("{}"), status: 204},
		"declared length too large": {contentType: "application/json", body: strings.NewReader(exact + "b"), status: 413, code: "request_too_large"},
		"exact bound":               {contentType: "application/json", body: strings.NewReader(exact), status: 204},
		"chunked body too large":    {contentType: "application/json", body: io.MultiReader(strings.NewReader(exact), strings.NewReader("b")), length: -1, status: 413, code: "request_too_large"},
		"chunked at the bound":      {contentType: "application/json", body: io.MultiReader(strings.NewReader(exact)), length: -1, status: 204},
		"unreadable body":           {contentType: "application/json", body: failingReader{}, length: -1, status: 400, code: "invalid_request"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newFixture(t, fixtureOptions{})
			issued := fixture.keys.add(t, principal.ClassTrusted, "main")
			req := request(t, http.MethodPost, routeChatCompletions, issued.Plaintext, tc.body)
			if tc.length != 0 {
				req.ContentLength = tc.length
			}
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			rec := fixture.do(req)
			if tc.status == http.StatusNoContent {
				if rec.Code != tc.status || len(fixture.chat.received()) != 1 {
					t.Fatalf("status = %d with %d chat calls, want accepted", rec.Code, len(fixture.chat.received()))
				}
				return
			}
			expectError(t, rec, tc.status, "invalid_request_error", tc.code)
			if len(fixture.chat.received()) != 0 {
				t.Error("chat handler ran for a rejected request")
			}
		})
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestHandlerConcurrencySlotsAreHeldPerKey(t *testing.T) {
	chat := &recordingChat{enter: make(chan struct{}), hold: make(chan struct{})}
	fixture := newFixture(t, fixtureOptions{concurrency: 1, chat: chat})
	first := fixture.keys.add(t, principal.ClassTrusted, "main")
	second := fixture.keys.add(t, principal.ClassTrusted, "main")

	held := make(chan *httptest.ResponseRecorder, 1)
	go func() { held <- fixture.do(jsonPost(t, first.Plaintext, "{}")) }()
	waitSignal(t, chat.enter)

	rec := fixture.do(jsonPost(t, first.Plaintext, "{}"))
	expectError(t, rec, 503, "server_error", "runtime_overloaded")
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}

	models := fixture.do(request(t, http.MethodGet, routeModels, first.Plaintext, nil))
	expectError(t, models, 503, "server_error", "runtime_overloaded")

	otherDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { otherDone <- fixture.do(jsonPost(t, second.Plaintext, "{}")) }()
	waitSignal(t, chat.enter)

	close(chat.hold)
	for _, done := range []chan *httptest.ResponseRecorder{held, otherDone} {
		select {
		case rec := <-done:
			if rec.Code != http.StatusNoContent {
				t.Fatalf("held request status = %d", rec.Code)
			}
		case <-time.After(testWait):
			t.Fatal("held request did not finish")
		}
	}
	if after := fixture.do(request(t, http.MethodGet, routeModels, first.Plaintext, nil)); after.Code != http.StatusOK {
		t.Fatalf("slot was not released: status %d", after.Code)
	}
	if len(fixture.handler.slots.active) != 0 {
		t.Errorf("slot table keeps %d idle entries", len(fixture.handler.slots.active))
	}
}

func waitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(testWait):
		t.Fatal("handler was not entered")
	}
}

func TestHandlerRateLimitRefillsWithTheClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newFixture(t, fixtureOptions{rate: 2})
		first := fixture.keys.add(t, principal.ClassTrusted, "main")
		second := fixture.keys.add(t, principal.ClassTrusted, "main")
		get := func(key IssuedKey) *httptest.ResponseRecorder {
			return fixture.do(request(t, http.MethodGet, routeModels, key.Plaintext, nil))
		}
		for i := range 2 {
			if rec := get(first); rec.Code != http.StatusOK {
				t.Fatalf("request %d status = %d", i, rec.Code)
			}
		}
		limited := get(first)
		expectError(t, limited, 429, "rate_limit_error", "rate_limit_exceeded")
		if got := limited.Header().Get("Retry-After"); got != "30" {
			t.Errorf("Retry-After = %q, want 30", got)
		}
		if rec := get(second); rec.Code != http.StatusOK {
			t.Fatalf("another key shares the bucket: status %d", rec.Code)
		}
		time.Sleep(15 * time.Second)
		half := get(first)
		expectError(t, half, 429, "rate_limit_error", "rate_limit_exceeded")
		if got := half.Header().Get("Retry-After"); got != "15" {
			t.Errorf("Retry-After after 15s = %q, want 15", got)
		}
		time.Sleep(15 * time.Second)
		if rec := get(first); rec.Code != http.StatusOK {
			t.Fatalf("bucket did not refill: status %d", rec.Code)
		}
	})
}

func TestHandlerRateLimitPrecedesContentTypeAndBodyChecks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newFixture(t, fixtureOptions{rate: 1})
		issued := fixture.keys.add(t, principal.ClassTrusted, "main")
		if rec := fixture.do(jsonPost(t, issued.Plaintext, "{}")); rec.Code != http.StatusNoContent {
			t.Fatalf("first request status = %d", rec.Code)
		}
		bad := request(t, http.MethodPost, routeChatCompletions, issued.Plaintext, strings.NewReader("{}"))
		bad.Header.Set("Content-Type", "text/plain")
		expectError(t, fixture.do(bad), 429, "rate_limit_error", "rate_limit_exceeded")
	})
}

func TestHandlerRejectedRequestsReleaseTheirSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newFixture(t, fixtureOptions{rate: 1, concurrency: 1})
		issued := fixture.keys.add(t, principal.ClassTrusted, "main")
		for i := range 3 {
			rec := fixture.do(request(t, http.MethodGet, routeModels, issued.Plaintext, nil))
			want := http.StatusOK
			if i > 0 {
				want = http.StatusTooManyRequests
			}
			if rec.Code != want {
				t.Fatalf("request %d status = %d, want %d", i, rec.Code, want)
			}
		}
		if len(fixture.handler.slots.active) != 0 {
			t.Errorf("slot table keeps %d entries after rate-limited requests", len(fixture.handler.slots.active))
		}
	})
}

func TestHandlerNeverLogsKeyMaterial(t *testing.T) {
	fixture := newFixture(t, fixtureOptions{})
	valid := fixture.keys.add(t, principal.ClassTrusted, "main")
	revoked := fixture.keys.add(t, principal.ClassTrusted, "main")
	fixture.keys.revoke(revoked.ID)
	unknown, err := NewKey()
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	wrongSecret := valid.Plaintext[:len(valid.Plaintext)-1] + "z"
	for _, token := range []string{revoked.Plaintext, unknown.Plaintext, wrongSecret, "aura_sk_" + strings.Repeat("a", 12) + "_secret-probe-81c4"} {
		fixture.do(request(t, http.MethodGet, routeModels, token, nil))
	}
	fixture.keys.failure = errors.New("backend down")
	fixture.do(request(t, http.MethodGet, routeModels, valid.Plaintext, nil))
	logged := fixture.logs.String()
	if logged == "" {
		t.Fatal("expected rejection logs")
	}
	for _, issued := range []IssuedKey{valid, revoked, unknown} {
		secret := issued.Plaintext[len(KeyPrefix)+KeyIDLength+1:]
		if strings.Contains(logged, secret) || strings.Contains(logged, issued.Plaintext) {
			t.Fatalf("logs leak key material:\n%s", logged)
		}
	}
	if strings.Contains(logged, "secret-probe-81c4") || strings.Contains(logged, wrongSecret) {
		t.Fatalf("logs leak a presented token:\n%s", logged)
	}
}

func TestNewHandlerValidatesOptions(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	good := func() *Options {
		return &Options{Keys: &memoryKeys{}, MaxBodyBytes: 1, RequestsPerMinute: 1, MaxConcurrentPerKey: 1, Logger: logger}
	}
	cases := map[string]func(*Options) *Options{
		"nil options":      func(*Options) *Options { return nil },
		"nil key store":    func(o *Options) *Options { o.Keys = nil; return o },
		"zero body bound":  func(o *Options) *Options { o.MaxBodyBytes = 0; return o },
		"zero rate":        func(o *Options) *Options { o.RequestsPerMinute = 0; return o },
		"zero concurrency": func(o *Options) *Options { o.MaxConcurrentPerKey = 0; return o },
		"nil logger":       func(o *Options) *Options { o.Logger = nil; return o },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			handler, err := NewHandler(mutate(good()))
			if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument || handler != nil {
				t.Fatalf("NewHandler = %v, %v; want %s", handler, err, ErrorCodeInvalidArgument)
			}
		})
	}
	if _, err := NewHandler(good()); err != nil {
		t.Fatalf("NewHandler(valid): %v", err)
	}
}
