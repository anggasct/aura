package httpchat

import (
	"bytes"
	"context"
	"encoding/json"
	"iter"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/channels/kit/access"
	"github.com/anggasct/aura/internal/channels/kit/approval"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/store"
)

const (
	testPersona   = "support"
	testReply     = "hello from the persona"
	testTurnID    = "turn_test"
	testUsageJSON = `{"promptTokenCount":10,"candidatesTokenCount":4,"thoughtsTokenCount":1}`

	testKeepalive = 15 * time.Second
)

type scriptedRuntime struct {
	mu        sync.Mutex
	turns     []turn.TurnRequest
	reply     string
	failure   string
	terminal  string
	runErr    error
	noUsage   bool
	cancelCtx context.CancelFunc
}

func (s *scriptedRuntime) Run(_ context.Context, req *turn.TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
	s.mu.Lock()
	s.turns = append(s.turns, *req)
	reply, failure, terminal, runErr, noUsage, cancel := s.reply, s.failure, s.terminal, s.runErr, s.noUsage, s.cancelCtx
	s.mu.Unlock()
	return func(yield func(store.RuntimeEvent, error) bool) {
		if runErr != nil {
			yield(store.RuntimeEvent{}, runErr)
			return
		}
		if !yield(store.RuntimeEvent{TurnID: testTurnID, Kind: turn.EventKindTurnAccepted}, nil) {
			return
		}
		if cancel != nil {
			cancel()
		}
		switch {
		case failure != "":
			payload := `{"code":"` + failure + `","detail":"internal detail with the prompt and secret model output"}`
			yield(store.RuntimeEvent{TurnID: testTurnID, Kind: turn.EventKindTurnFailed, Payload: json.RawMessage(payload)}, nil)
			return
		case terminal != "":
			yield(store.RuntimeEvent{TurnID: testTurnID, Kind: terminal}, nil)
			return
		}
		answer := store.RuntimeEvent{TurnID: testTurnID, Kind: turn.EventKindMessageCompleted}
		answer.Payload, _ = json.Marshal(map[string]string{"text": reply})
		if !noUsage {
			answer.ProviderUsage = json.RawMessage(testUsageJSON)
		}
		if !yield(answer, nil) {
			return
		}
		yield(store.RuntimeEvent{TurnID: testTurnID, Kind: turn.EventKindTurnCompleted}, nil)
	}
}

func (s *scriptedRuntime) requests() []turn.TurnRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]turn.TurnRequest(nil), s.turns...)
}

func testAnswerTexts(events []store.RuntimeEvent) []string {
	var texts []string
	for i := range events {
		if events[i].Kind != turn.EventKindMessageCompleted {
			continue
		}
		var decoded struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(events[i].Payload, &decoded); err == nil {
			texts = append(texts, decoded.Text)
		}
	}
	return texts
}

type memorySessions struct {
	mu        sync.Mutex
	sessions  map[string]Session
	created   []Session
	getErr    error
	createErr error
}

func (m *memorySessions) Get(_ context.Context, id string) (Session, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getErr != nil {
		return Session{}, false, m.getErr
	}
	found, ok := m.sessions[id]
	return found, ok, nil
}

func (m *memorySessions) Create(_ context.Context, session *Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.createErr != nil {
		return m.createErr
	}
	if m.sessions == nil {
		m.sessions = map[string]Session{}
	}
	m.sessions[session.ID] = *session
	m.created = append(m.created, *session)
	return nil
}

func (m *memorySessions) createdSessions() []Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Session(nil), m.created...)
}

func (m *memorySessions) put(session Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions == nil {
		m.sessions = map[string]Session{}
	}
	m.sessions[session.ID] = session
}

type staticPersonas map[string]agent.Definition

func (p staticPersonas) Lookup(id string) (agent.Definition, bool) {
	definition, ok := p[id]
	return definition, ok
}

type gateLog struct {
	mu     sync.Mutex
	calls  []string
	denied []string
}

func (g *gateLog) record(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, name)
}

func (g *gateLog) order() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.calls...)
}

func (g *gateLog) reasons() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.denied...)
}

type fakeBudgets struct {
	log     *gateLog
	refusal string
	err     error
}

func (f *fakeBudgets) Admit(_ context.Context, principalID string, class principal.Class) (string, error) {
	f.log.record("budget:" + principalID + ":" + string(class))
	return f.refusal, f.err
}

type fakeEvals struct {
	log       *gateLog
	evaluated bool
	err       error
}

func (f *fakeEvals) Evaluated(_ context.Context, persona string) (bool, error) {
	f.log.record("eval:" + persona)
	return f.evaluated, f.err
}

type fakeHandoffs struct {
	log   *gateLog
	state string
	err   error
}

func (f *fakeHandoffs) Check(_ context.Context, sessionID string) (string, error) {
	f.log.record("handoff:" + sessionID)
	if f.state == "" {
		return access.HandoffStateNone, f.err
	}
	return f.state, f.err
}

type chatFixture struct {
	handler  *Handler
	keys     *memoryKeys
	runtime  *scriptedRuntime
	sessions *memorySessions
	personas staticPersonas
	budgets  *fakeBudgets
	evals    *fakeEvals
	handoffs *fakeHandoffs
	gates    *gateLog
	logs     *bytes.Buffer
}

type chatFixtureOptions struct {
	maxMessages int
	maxBytes    int
	audience    string
	backend     turn.AgentRuntime
	approvals   *approval.OwnerRoute
	answers     func(events []store.RuntimeEvent) []string
}

func newChatFixture(t *testing.T, opts chatFixtureOptions) *chatFixture {
	t.Helper()
	gates := &gateLog{}
	logs := &bytes.Buffer{}
	audience := opts.audience
	if audience == "" {
		audience = config.AudiencePublic
	}
	fixture := &chatFixture{
		keys:     &memoryKeys{},
		runtime:  &scriptedRuntime{reply: testReply},
		sessions: &memorySessions{},
		personas: staticPersonas{testPersona: {ID: testPersona, Audience: audience}},
		budgets:  &fakeBudgets{log: gates},
		evals:    &fakeEvals{log: gates, evaluated: true},
		handoffs: &fakeHandoffs{log: gates},
		gates:    gates,
		logs:     logs,
	}
	logger := slog.New(slog.NewTextHandler(&lockedWriter{w: logs}, nil))
	var backend turn.AgentRuntime = fixture.runtime
	if opts.backend != nil {
		backend = opts.backend
	}
	answers := testAnswerTexts
	if opts.answers != nil {
		answers = opts.answers
	}
	chat, err := NewChat(&ChatOptions{
		Runtime:   backend,
		Sessions:  fixture.sessions,
		Personas:  fixture.personas,
		Budgets:   fixture.budgets,
		Evals:     fixture.evals,
		Handoffs:  fixture.handoffs,
		Approvals: opts.approvals,
		Denied: func(_ context.Context, reason string) {
			gates.mu.Lock()
			defer gates.mu.Unlock()
			gates.denied = append(gates.denied, reason)
		},
		AnswerTexts:        answers,
		MaxHistoryMessages: opts.maxMessages,
		MaxHistoryBytes:    opts.maxBytes,
		KeepaliveInterval:  testKeepalive,
		Logger:             logger,
	})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	handler, err := NewHandler(&Options{
		Keys:                fixture.keys,
		Chat:                chat,
		MaxBodyBytes:        1 << 20,
		RequestsPerMinute:   600,
		MaxConcurrentPerKey: 4,
		Logger:              logger,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	fixture.handler = handler
	return fixture
}

func (f *chatFixture) post(t *testing.T, key, body string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := jsonPost(t, key, body)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Add(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func chatBody(messages ...string) string {
	return `{"model":"` + testPersona + `","messages":[` + strings.Join(messages, ",") + `]}`
}

func userMessage(text string) string {
	return `{"role":"user","content":` + jsonString(text) + `}`
}

func roleMessage(role, text string) string {
	return `{"role":"` + role + `","content":` + jsonString(text) + `}`
}

func jsonString(text string) string {
	encoded, err := json.Marshal(text)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

type completionBody struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
	} `json:"usage"`
}

func decodeCompletion(t *testing.T, rec *httptest.ResponseRecorder) completionBody {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var decoded completionBody
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode completion %q: %v", rec.Body.String(), err)
	}
	return decoded
}

func expectCoded(t *testing.T, rec *httptest.ResponseRecorder, status int, kind, code, param string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, status, rec.Body.String())
	}
	decoded := decodeEnvelope(t, rec)
	gotParam := ""
	if decoded.Error.Param != nil {
		gotParam = *decoded.Error.Param
	}
	if decoded.Error.Type != kind || decoded.Error.Code != code || decoded.Error.Message == "" || gotParam != param {
		t.Fatalf("envelope = %+v param %q, want type %s code %s param %q", decoded.Error, gotParam, kind, code, param)
	}
}

type probeKey struct{}

type contextProbe struct {
	*scriptedRuntime
	mu     sync.Mutex
	marker any
	err    error
}

func (p *contextProbe) Run(ctx context.Context, req *turn.TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
	p.mu.Lock()
	p.marker, p.err = ctx.Value(probeKey{}), ctx.Err()
	p.mu.Unlock()
	return p.scriptedRuntime.Run(ctx, req)
}

func slogDiscard() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}
