package httpchat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/anggasct/aura/internal/channel/access"
	"github.com/anggasct/aura/internal/principal"
)

type fakeReplySource struct {
	mu      sync.Mutex
	stored  []HandoffReply
	err     error
	queries []replyQuery
}

type replyQuery struct {
	sessionID string
	after     uint64
	limit     int
}

func (f *fakeReplySource) List(_ context.Context, sessionID string, afterSequence uint64, limit int) ([]HandoffReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, replyQuery{sessionID: sessionID, after: afterSequence, limit: limit})
	if f.err != nil {
		return nil, f.err
	}
	var listed []HandoffReply
	for _, reply := range f.stored {
		if reply.Sequence > afterSequence && len(listed) < limit {
			listed = append(listed, reply)
		}
	}
	return listed, nil
}

func (f *fakeReplySource) recorded() []replyQuery {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]replyQuery(nil), f.queries...)
}

type repliesFixture struct {
	handler  *Handler
	keys     *memoryKeys
	sessions *memorySessions
	handoffs *fakeHandoffs
	source   *fakeReplySource
	logs     *bytes.Buffer
}

func newRepliesFixture(t *testing.T, rate int) *repliesFixture {
	t.Helper()
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(&lockedWriter{w: logs}, nil))
	fixture := &repliesFixture{
		keys:     &memoryKeys{},
		sessions: &memorySessions{},
		handoffs: &fakeHandoffs{log: &gateLog{}},
		source:   &fakeReplySource{},
		logs:     logs,
	}
	replies, err := NewReplies(&RepliesOptions{Sessions: fixture.sessions, Handoffs: fixture.handoffs, Replies: fixture.source, Logger: logger})
	if err != nil {
		t.Fatalf("NewReplies: %v", err)
	}
	handler, err := NewHandler(&Options{
		Keys:                fixture.keys,
		Replies:             replies,
		MaxBodyBytes:        testBodyLimit,
		RequestsPerMinute:   cmpOr(rate, 600),
		MaxConcurrentPerKey: 4,
		Logger:              logger,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	fixture.handler = handler
	return fixture
}

func (f *repliesFixture) session(key IssuedKey) string {
	id := testPersona + "/http/" + key.ID + "/" + strings.Repeat("a", sessionTokenLength)
	f.sessions.put(Session{ID: id, OwnerID: "http:" + key.ID, PersonaID: testPersona})
	return id
}

func (f *repliesFixture) get(t *testing.T, key, target string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := request(t, http.MethodGet, target, key, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Add(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

type decodedReplies struct {
	Object string `json:"object"`
	Data   []struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Content string `json:"content"`
		Created int64  `json:"created"`
	} `json:"data"`
	HandoffOpen bool `json:"handoff_open"`
}

func decodeReplies(t *testing.T, rec *httptest.ResponseRecorder) decodedReplies {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var decoded decodedReplies
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode replies %q: %v", rec.Body.String(), err)
	}
	return decoded
}

func storedReplies(count int) []HandoffReply {
	replies := make([]HandoffReply, 0, count)
	for i := range count {
		replies = append(replies, HandoffReply{
			Sequence:  uint64(2 * (i + 1)),
			Text:      "reply " + strconv.Itoa(i+1),
			CreatedAt: testCreated.Add(time.Duration(i) * time.Minute),
		})
	}
	return replies
}

func TestRepliesListsOwnerRepliesOldestFirst(t *testing.T) {
	fixture := newRepliesFixture(t, 0)
	fixture.source.stored = storedReplies(3)
	fixture.handoffs.state = access.HandoffStateOpen
	issued := fixture.keys.add(t, principal.ClassPublic, testPersona)
	session := fixture.session(issued)

	rec := fixture.get(t, issued.Plaintext, routeHandoffReplies, SessionHeader, session)

	decoded := decodeReplies(t, rec)
	if decoded.Object != "list" || !decoded.HandoffOpen || len(decoded.Data) != 3 {
		t.Fatalf("replies = %+v, want an open list of 3", decoded)
	}
	for i, item := range decoded.Data {
		wantID := strconv.Itoa(2 * (i + 1))
		wantContent := "reply " + strconv.Itoa(i+1)
		wantCreated := testCreated.Add(time.Duration(i) * time.Minute).Unix()
		if item.ID != wantID || item.Object != "handoff.reply" || item.Content != wantContent || item.Created != wantCreated {
			t.Errorf("data[%d] = %+v, want id %s content %q created %d", i, item, wantID, wantContent, wantCreated)
		}
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if queries := fixture.source.recorded(); len(queries) != 1 || queries[0] != (replyQuery{sessionID: session, after: 0, limit: 100}) {
		t.Errorf("source queries = %+v, want one bounded query for the session", queries)
	}
}

func TestRepliesCursorAndHandoffState(t *testing.T) {
	cases := []struct {
		name     string
		target   string
		state    string
		wantIDs  []string
		wantOpen bool
	}{
		{name: "after skips seen replies", target: routeHandoffReplies + "?after=2", state: access.HandoffStateOpen, wantIDs: []string{"4", "6"}, wantOpen: true},
		{name: "after the last reply is empty", target: routeHandoffReplies + "?after=6", state: access.HandoffStateOpen, wantIDs: []string{}, wantOpen: true},
		{name: "released handoff still lists replies", target: routeHandoffReplies, state: access.HandoffStateNone, wantIDs: []string{"2", "4", "6"}},
		{name: "expired handoff reports closed", target: routeHandoffReplies, state: access.HandoffStateExpired, wantIDs: []string{"2", "4", "6"}},
		{name: "maximum cursor is accepted", target: routeHandoffReplies + "?after=9223372036854775807", state: access.HandoffStateNone, wantIDs: []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newRepliesFixture(t, 0)
			fixture.source.stored = storedReplies(3)
			fixture.handoffs.state = tc.state
			issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)

			decoded := decodeReplies(t, fixture.get(t, issued.Plaintext, tc.target, SessionHeader, fixture.session(issued)))

			ids := make([]string, 0, len(decoded.Data))
			for _, item := range decoded.Data {
				ids = append(ids, item.ID)
			}
			if strings.Join(ids, ",") != strings.Join(tc.wantIDs, ",") || decoded.HandoffOpen != tc.wantOpen {
				t.Fatalf("ids = %v open = %v, want %v open = %v", ids, decoded.HandoffOpen, tc.wantIDs, tc.wantOpen)
			}
		})
	}
}

func TestRepliesEmptyListIsAnArray(t *testing.T) {
	fixture := newRepliesFixture(t, 0)
	issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)

	rec := fixture.get(t, issued.Plaintext, routeHandoffReplies, SessionHeader, fixture.session(issued))

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"data":[]`) {
		t.Fatalf("body = %s, want an empty data array", rec.Body.String())
	}
}

func TestRepliesReturnAtMostOneHundred(t *testing.T) {
	fixture := newRepliesFixture(t, 0)
	fixture.source.stored = storedReplies(120)
	issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)
	session := fixture.session(issued)

	first := decodeReplies(t, fixture.get(t, issued.Plaintext, routeHandoffReplies, SessionHeader, session))
	if len(first.Data) != 100 || first.Data[99].ID != "200" {
		t.Fatalf("first page = %d items ending at %q, want 100 ending at 200", len(first.Data), first.Data[len(first.Data)-1].ID)
	}
	second := decodeReplies(t, fixture.get(t, issued.Plaintext, routeHandoffReplies+"?after="+first.Data[99].ID, SessionHeader, session))
	if len(second.Data) != 20 || second.Data[0].ID != "202" {
		t.Fatalf("second page = %d items starting at %q, want 20 starting at 202", len(second.Data), second.Data[0].ID)
	}
}

func TestRepliesRejectionsFollowTheDocumentedErrors(t *testing.T) {
	cases := []struct {
		name    string
		target  string
		headers func(session string) []string
		status  int
		kind    string
		code    string
		param   string
	}{
		{
			name: "missing session header", target: routeHandoffReplies,
			headers: func(string) []string { return nil },
			status:  http.StatusBadRequest, kind: errorTypeInvalidRequest, code: "invalid_request", param: paramSessionHeader,
		},
		{
			name: "empty session header", target: routeHandoffReplies,
			headers: func(string) []string { return []string{SessionHeader, ""} },
			status:  http.StatusBadRequest, kind: errorTypeInvalidRequest, code: "invalid_request", param: paramSessionHeader,
		},
		{
			name: "repeated session header", target: routeHandoffReplies,
			headers: func(session string) []string { return []string{SessionHeader, session, SessionHeader, session} },
			status:  http.StatusBadRequest, kind: errorTypeInvalidRequest, code: "invalid_request", param: paramSessionHeader,
		},
		{
			name: "non decimal cursor", target: routeHandoffReplies + "?after=abc",
			headers: func(session string) []string { return []string{SessionHeader, session} },
			status:  http.StatusBadRequest, kind: errorTypeInvalidRequest, code: "invalid_request", param: paramAfter,
		},
		{
			name: "signed cursor", target: routeHandoffReplies + "?after=%2B5",
			headers: func(session string) []string { return []string{SessionHeader, session} },
			status:  http.StatusBadRequest, kind: errorTypeInvalidRequest, code: "invalid_request", param: paramAfter,
		},
		{
			name: "negative cursor", target: routeHandoffReplies + "?after=-1",
			headers: func(session string) []string { return []string{SessionHeader, session} },
			status:  http.StatusBadRequest, kind: errorTypeInvalidRequest, code: "invalid_request", param: paramAfter,
		},
		{
			name: "empty cursor", target: routeHandoffReplies + "?after=",
			headers: func(session string) []string { return []string{SessionHeader, session} },
			status:  http.StatusBadRequest, kind: errorTypeInvalidRequest, code: "invalid_request", param: paramAfter,
		},
		{
			name: "oversized cursor", target: routeHandoffReplies + "?after=9223372036854775808",
			headers: func(session string) []string { return []string{SessionHeader, session} },
			status:  http.StatusBadRequest, kind: errorTypeInvalidRequest, code: "invalid_request", param: paramAfter,
		},
		{
			name: "repeated cursor", target: routeHandoffReplies + "?after=1&after=2",
			headers: func(session string) []string { return []string{SessionHeader, session} },
			status:  http.StatusBadRequest, kind: errorTypeInvalidRequest, code: "invalid_request", param: paramAfter,
		},
		{
			name: "malformed session id", target: routeHandoffReplies,
			headers: func(string) []string { return []string{SessionHeader, "not-a-session"} },
			status:  http.StatusNotFound, kind: errorTypeInvalidRequest, code: "session_not_found", param: paramSessionHeader,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newRepliesFixture(t, 0)
			fixture.source.stored = storedReplies(1)
			issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)

			rec := fixture.get(t, issued.Plaintext, tc.target, tc.headers(fixture.session(issued))...)

			expectCoded(t, rec, tc.status, tc.kind, tc.code, tc.param)
			if queries := fixture.source.recorded(); len(queries) != 0 {
				t.Errorf("a rejected request read replies: %+v", queries)
			}
		})
	}
}

func TestRepliesNeverReadSessionsThePrincipalDoesNotOwn(t *testing.T) {
	fixture := newRepliesFixture(t, 0)
	fixture.source.stored = storedReplies(2)
	mine := fixture.keys.add(t, principal.ClassTrusted, testPersona)
	other := fixture.keys.add(t, principal.ClassTrusted, testPersona)
	foreign := fixture.session(other)
	unknown := testPersona + "/http/" + mine.ID + "/" + strings.Repeat("b", sessionTokenLength)
	wrongOwner := testPersona + "/http/" + mine.ID + "/" + strings.Repeat("c", sessionTokenLength)
	fixture.sessions.put(Session{ID: wrongOwner, OwnerID: "http:" + other.ID, PersonaID: testPersona})
	wrongPersona := testPersona + "/http/" + mine.ID + "/" + strings.Repeat("d", sessionTokenLength)
	fixture.sessions.put(Session{ID: wrongPersona, OwnerID: "http:" + mine.ID, PersonaID: "elsewhere"})

	for name, session := range map[string]string{"another key's session": foreign, "unknown session": unknown, "session owned by another principal": wrongOwner, "session of another persona": wrongPersona} {
		t.Run(name, func(t *testing.T) {
			rec := fixture.get(t, mine.Plaintext, routeHandoffReplies, SessionHeader, session)
			expectCoded(t, rec, http.StatusNotFound, errorTypeInvalidRequest, "session_not_found", paramSessionHeader)
		})
	}
	if queries := fixture.source.recorded(); len(queries) != 0 {
		t.Errorf("replies were read for sessions the key does not own: %+v", queries)
	}
	if calls := fixture.handoffs.log.order(); len(calls) != 0 {
		t.Errorf("handoff gate consulted for a foreign session: %v", calls)
	}
}

func TestRepliesLookupFailuresDenyWithAStaticMessage(t *testing.T) {
	cases := map[string]func(f *repliesFixture){
		"session store":  func(f *repliesFixture) { f.sessions.getErr = errors.New("db down with secret reply text") },
		"reply source":   func(f *repliesFixture) { f.source.err = errors.New("db down with secret reply text") },
		"handoff gate":   func(f *repliesFixture) { f.handoffs.err = errors.New("db down with secret reply text") },
		"reply and gate": func(f *repliesFixture) { f.source.err, f.handoffs.err = errors.New("one"), errors.New("two") },
	}
	for name, fail := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newRepliesFixture(t, 0)
			fixture.source.stored = storedReplies(1)
			issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)
			session := fixture.session(issued)
			fail(fixture)

			rec := fixture.get(t, issued.Plaintext, routeHandoffReplies, SessionHeader, session)

			expectCoded(t, rec, http.StatusInternalServerError, errorTypeServer, "internal_error", "")
			if strings.Contains(rec.Body.String(), "secret") {
				t.Errorf("internal detail leaked into the response: %s", rec.Body.String())
			}
		})
	}
}

func TestRepliesNeverLogReplyText(t *testing.T) {
	fixture := newRepliesFixture(t, 0)
	fixture.source.stored = []HandoffReply{{Sequence: 2, Text: "private owner words", CreatedAt: testCreated}}
	issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)
	session := fixture.session(issued)

	decodeReplies(t, fixture.get(t, issued.Plaintext, routeHandoffReplies, SessionHeader, session))
	fixture.get(t, issued.Plaintext, routeHandoffReplies+"?after=oops", SessionHeader, session)

	if strings.Contains(fixture.logs.String(), "private owner words") {
		t.Errorf("reply text reached the logs: %s", fixture.logs.String())
	}
}

func TestRepliesRouteSharesTheHandlerPipeline(t *testing.T) {
	t.Run("authentication", func(t *testing.T) {
		fixture := newRepliesFixture(t, 0)
		issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)
		session := fixture.session(issued)
		fixture.keys.revoke(issued.ID)
		for name, bearer := range map[string]string{"no key": "", "revoked key": issued.Plaintext, "malformed key": "nonsense"} {
			t.Run(name, func(t *testing.T) {
				rec := fixture.get(t, bearer, routeHandoffReplies, SessionHeader, session)
				expectError(t, rec, http.StatusUnauthorized, errorTypeAuthentication, "invalid_api_key")
			})
		}
		if queries := fixture.source.recorded(); len(queries) != 0 {
			t.Errorf("unauthenticated requests read replies: %+v", queries)
		}
	})
	t.Run("method", func(t *testing.T) {
		fixture := newRepliesFixture(t, 0)
		issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodHead} {
			rec := httptest.NewRecorder()
			fixture.handler.ServeHTTP(rec, request(t, method, routeHandoffReplies, issued.Plaintext, nil))
			expectError(t, rec, http.StatusMethodNotAllowed, errorTypeInvalidRequest, "method_not_allowed")
			if got := rec.Header().Get("Allow"); got != http.MethodGet {
				t.Errorf("%s Allow = %q, want GET", method, got)
			}
		}
		if lookups := fixture.keys.lookups.Load(); lookups != 0 {
			t.Errorf("key store was consulted %d times before the method check", lookups)
		}
	})
	t.Run("not mounted without a replies handler", func(t *testing.T) {
		fixture := newFixture(t, fixtureOptions{})
		issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)
		rec := fixture.do(request(t, http.MethodGet, routeHandoffReplies, issued.Plaintext, nil))
		expectError(t, rec, http.StatusNotFound, errorTypeInvalidRequest, "not_found")
	})
	t.Run("rate limit", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			fixture := newRepliesFixture(t, 2)
			issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)
			session := fixture.session(issued)
			for i := range 2 {
				if rec := fixture.get(t, issued.Plaintext, routeHandoffReplies, SessionHeader, session); rec.Code != http.StatusOK {
					t.Fatalf("request %d status = %d", i, rec.Code)
				}
			}
			limited := fixture.get(t, issued.Plaintext, routeHandoffReplies, SessionHeader, session)
			expectError(t, limited, http.StatusTooManyRequests, errorTypeRateLimit, "rate_limit_exceeded")
			time.Sleep(30 * time.Second)
			if rec := fixture.get(t, issued.Plaintext, routeHandoffReplies, SessionHeader, session); rec.Code != http.StatusOK {
				t.Fatalf("bucket did not refill: status %d", rec.Code)
			}
		})
	})
}

func TestNewRepliesValidatesOptions(t *testing.T) {
	valid := func() RepliesOptions {
		return RepliesOptions{Sessions: &memorySessions{}, Handoffs: &fakeHandoffs{log: &gateLog{}}, Replies: &fakeReplySource{}, Logger: slog.New(slog.DiscardHandler)}
	}
	cases := map[string]func(o *RepliesOptions){
		"sessions": func(o *RepliesOptions) { o.Sessions = nil },
		"handoffs": func(o *RepliesOptions) { o.Handoffs = nil },
		"replies":  func(o *RepliesOptions) { o.Replies = nil },
		"logger":   func(o *RepliesOptions) { o.Logger = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			opts := valid()
			mutate(&opts)
			if _, err := NewReplies(&opts); err == nil {
				t.Fatalf("NewReplies accepted options without %s", name)
			}
		})
	}
	if _, err := NewReplies(nil); err == nil {
		t.Fatal("NewReplies accepted nil options")
	}
	opts := valid()
	if _, err := NewReplies(&opts); err != nil {
		t.Fatalf("NewReplies rejected valid options: %v", err)
	}
}
