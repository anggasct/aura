package telegram

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/effect"
	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/runtime"
	runtimeingress "github.com/anggasct/aura/internal/runtime/ingress"
	"github.com/anggasct/aura/internal/store"
)

const (
	testToken    = "123456:tok-secret-probe-9f8e7d"
	testTokenEnv = "AURA_TEST_TELEGRAM_TOKEN"
	ownerUserID  = 424242
	strangerID   = 999001
	testPersona  = "main"
)

type botCall struct {
	method string
	body   map[string]any
}

type fakeBotAPI struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	batches [][]update
	calls   []botCall
	replies []func(w http.ResponseWriter) bool
	nextID  int64
	polled  chan int64
	hold    bool
}

func newFakeBotAPI(t *testing.T) *fakeBotAPI {
	t.Helper()
	api := &fakeBotAPI{t: t, nextID: 700, polled: make(chan int64, 64)}
	api.srv = httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(api.srv.Close)
	return api
}

func (f *fakeBotAPI) serve(w http.ResponseWriter, r *http.Request) {
	method, ok := strings.CutPrefix(r.URL.Path, "/bot"+testToken+"/")
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"ok":false,"error_code":404,"description":"Not Found"}`)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.calls = append(f.calls, botCall{method: method, body: body})
	var reply func(w http.ResponseWriter) bool
	if len(f.replies) > 0 {
		reply = f.replies[0]
		f.replies = f.replies[1:]
	}
	f.mu.Unlock()
	if reply != nil && reply(w) {
		return
	}
	switch method {
	case "getUpdates":
		offset, _ := body["offset"].(float64)
		select {
		case f.polled <- int64(offset):
		case <-r.Context().Done():
			return
		}
		f.mu.Lock()
		var batch []update
		if len(f.batches) > 0 {
			batch = f.batches[0]
			f.batches = f.batches[1:]
		}
		hold := f.hold && batch == nil
		f.mu.Unlock()
		if hold {
			<-r.Context().Done()
			return
		}
		if batch == nil {
			select {
			case <-time.After(20 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
			batch = []update{}
		}
		writeOK(w, batch)
	case "sendMessage":
		f.mu.Lock()
		f.nextID++
		id := f.nextID
		f.mu.Unlock()
		writeOK(w, map[string]any{"message_id": id})
	default:
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"unknown method"}`)
	}
}

func writeOK(w http.ResponseWriter, result any) {
	raw, _ := json.Marshal(map[string]any{"ok": true, "result": result})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

func apiFailure(status, retryAfter int) func(w http.ResponseWriter) bool {
	return func(w http.ResponseWriter) bool {
		payload := map[string]any{"ok": false, "error_code": status, "description": "failure"}
		if retryAfter > 0 {
			payload["parameters"] = map[string]any{"retry_after": retryAfter}
		}
		raw, _ := json.Marshal(payload)
		w.WriteHeader(status)
		_, _ = w.Write(raw)
		return true
	}
}

func (f *fakeBotAPI) queue(batch ...update) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches = append(f.batches, batch)
}

func (f *fakeBotAPI) failNext(reply func(w http.ResponseWriter) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies = append(f.replies, reply)
}

func (f *fakeBotAPI) sends() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, call := range f.calls {
		if call.method == "sendMessage" {
			out = append(out, call.body)
		}
	}
	return out
}

func (f *fakeBotAPI) awaitPoll(t *testing.T) int64 {
	t.Helper()
	select {
	case offset := <-f.polled:
		return offset
	case <-time.After(5 * time.Second):
		t.Fatal("no getUpdates call within 5s")
		return 0
	}
}

func privateText(updateID, userID int64, text string) update {
	return update{UpdateID: updateID, Message: &message{
		MessageID: updateID * 10,
		From:      &user{ID: userID},
		Chat:      chat{ID: userID, Type: chatTypePrivate},
		Text:      text,
	}}
}

type memorySink struct {
	mu        sync.Mutex
	calls     map[string]int
	turns     map[string]int
	envelopes []*runtimeingress.IngressEnvelope
	fail      map[string]error
}

func newMemorySink() *memorySink {
	return &memorySink{calls: map[string]int{}, turns: map[string]int{}, fail: map[string]error{}}
}

func (s *memorySink) Accept(_ context.Context, env *runtimeingress.IngressEnvelope) (runtimeingress.TurnRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[env.ExternalID]++
	if err, ok := s.fail[env.ExternalID]; ok {
		delete(s.fail, env.ExternalID)
		return runtimeingress.TurnRef{}, err
	}
	if s.turns[env.ExternalID] > 0 {
		return runtimeingress.TurnRef{TurnID: "turn-" + env.ExternalID, SessionID: env.ConversationID, Replayed: true}, nil
	}
	s.turns[env.ExternalID] = 1
	s.envelopes = append(s.envelopes, env)
	return runtimeingress.TurnRef{TurnID: "turn-" + env.ExternalID, SessionID: env.ConversationID}, nil
}

func (s *memorySink) failOnce(externalID string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail[externalID] = err
}

func (s *memorySink) stats(externalID string) (calls, turns int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[externalID], s.turns[externalID]
}

func (s *memorySink) accepted() []*runtimeingress.IngressEnvelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*runtimeingress.IngressEnvelope(nil), s.envelopes...)
}

type fixedRouter struct{}

func (fixedRouter) Resolve(scope *auraagent.RouteScope) (auraagent.RouteMatch, bool) {
	if scope.Channel != config.BindingChannelTelegram || scope.Account != "pocket" {
		return auraagent.RouteMatch{}, false
	}
	return auraagent.RouteMatch{Persona: testPersona, Score: auraagent.ScoreAccountDefault, Binding: auraagent.AccountDefaultBinding}, true
}

type storeSessions struct {
	sessions store.SessionService
}

func (s storeSessions) EnsureSession(ctx context.Context, sessionID, ownerID, agentID string) error {
	if _, err := s.sessions.Get(ctx, sessionID); err == nil {
		return nil
	}
	now := time.Now().UTC()
	return s.sessions.Create(ctx, &store.Session{ID: sessionID, OwnerID: ownerID, AgentID: agentID, Metadata: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now})
}

type failingCursors struct {
	CursorStore
	mu      sync.Mutex
	failing bool
}

func (c *failingCursors) Advance(ctx context.Context, source string, next int64, at time.Time) error {
	c.mu.Lock()
	failing := c.failing
	c.mu.Unlock()
	if failing {
		return store.Errorf(store.ErrorCodeInvalidArgument, "cursor write blocked")
	}
	return c.CursorStore.Advance(ctx, source, next, at)
}

type logCapture struct {
	mu sync.Mutex
	sb strings.Builder
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sb.Write(p)
}

func (c *logCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sb.String()
}

type harness struct {
	db       *sql.DB
	api      *fakeBotAPI
	cursors  CursorStore
	effects  *effect.Executor
	sessions storeSessions
	logs     *logCapture
	denials  []string
	mu       sync.Mutex
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	t.Setenv(testTokenEnv, testToken)
	ctx := t.Context()
	db, err := store.OpenDB(ctx, filepath.Join(t.TempDir(), "aura.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	journal, err := effect.NewJournal(db, effect.Options{Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}
	executor, err := effect.NewExecutor(journal)
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	return &harness{
		db:       db,
		api:      newFakeBotAPI(t),
		cursors:  store.NewTelegramCursorStore(db),
		effects:  executor,
		sessions: storeSessions{sessions: store.NewSessionService(db)},
		logs:     &logCapture{},
	}
}

func (h *harness) adapter(t *testing.T, cursors CursorStore) *Adapter {
	t.Helper()
	if cursors == nil {
		cursors = h.cursors
	}
	cfg := &config.Telegram{
		Enabled: true,
		Polling: config.TelegramPolling{Timeout: config.Duration(time.Second), MaxBackoff: config.Duration(time.Second)},
	}
	account := &config.TelegramAccount{Name: "pocket", BotTokenRef: "env://" + testTokenEnv}
	logger := slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	adapter, err := New(cfg, account, cursors, h.effects, h.sessions, logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	owners, err := principal.NewResolver([]string{"telegram:424242"})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	adapter.SetPrincipalResolver(owners)
	adapter.SetPersonaRouter(fixedRouter{})
	adapter.SetDenialObserver(func(_ context.Context, reason string) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.denials = append(h.denials, reason)
	})
	adapter.bot.base = h.api.srv.URL
	adapter.retryBase = 10 * time.Millisecond
	adapter.rateCap = 50 * time.Millisecond
	return adapter
}

func (h *harness) denied() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.denials...)
}

func startAdapter(t *testing.T, adapter *Adapter, sink runtimeingress.IngressSink) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- adapter.Start(ctx, sink) }()
	return func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Start(): %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Start() did not return after cancel")
		}
	}
}

func overloaded() error {
	return &runtime.Error{Code: runtime.ErrorCodeRuntimeOverloaded, Detail: "queue full"}
}
