package discord

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"github.com/anggasct/aura/internal/config"
	runtimeingress "github.com/anggasct/aura/internal/runtime/ingress"
)

const testToken = "tok-secret-probe-9f8e7d"

var errDialBlocked = errors.New("dial blocked")

type memorySink struct {
	mu    sync.Mutex
	calls map[string]int
	turns map[string]int
	order []string
}

func newMemorySink() *memorySink {
	return &memorySink{calls: map[string]int{}, turns: map[string]int{}}
}

func (s *memorySink) Accept(ctx context.Context, env *runtimeingress.IngressEnvelope) (runtimeingress.TurnRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[env.ExternalID]++
	s.order = append(s.order, env.ExternalID)
	if s.turns[env.ExternalID] > 0 {
		return runtimeingress.TurnRef{TurnID: "turn-" + env.ExternalID, SessionID: env.ConversationID, Replayed: true}, nil
	}
	s.turns[env.ExternalID] = 1
	return runtimeingress.TurnRef{TurnID: "turn-" + env.ExternalID, SessionID: env.ConversationID}, nil
}

func (s *memorySink) stats(id string) (calls, turns int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[id], s.turns[id]
}

type memoryResumeStore struct {
	mu     sync.Mutex
	cursor ResumeCursor
	found  bool
	saves  int
}

func (s *memoryResumeStore) Save(ctx context.Context, cursor *ResumeCursor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cursor = *cursor
	s.found = true
	s.saves++
	return nil
}

func (s *memoryResumeStore) Load(ctx context.Context) (ResumeCursor, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursor, s.found, nil
}

func (s *memoryResumeStore) Delete(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.found = false
	s.cursor = ResumeCursor{}
	return nil
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

type fakeGateway struct {
	t          *testing.T
	srv        *httptest.Server
	self       *httptest.Server
	selfStatus atomic.Int32
	url        string
	conns      chan *websocket.Conn
	frames     chan map[string]any
	mu         sync.Mutex
	autoAck    bool
}

func newFakeGateway(t *testing.T, autoAck bool) *fakeGateway {
	t.Helper()
	gateway := &fakeGateway{
		t:       t,
		conns:   make(chan *websocket.Conn, 8),
		frames:  make(chan map[string]any, 64),
		autoAck: autoAck,
	}
	gateway.selfStatus.Store(http.StatusOK)
	gateway.self = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/@me" {
			http.NotFound(w, r)
			return
		}
		if gateway.selfStatus.Load() != http.StatusOK {
			w.WriteHeader(int(gateway.selfStatus.Load()))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"999","bot":true}`))
	}))
	t.Cleanup(gateway.self.Close)
	gateway.srv = httptest.NewServer(websocket.Server{
		Handshake: func(*websocket.Config, *http.Request) error { return nil },
		Handler:   gateway.handle,
	})
	t.Cleanup(gateway.srv.Close)
	gateway.url = "ws://" + gateway.srv.Listener.Addr().String() + "/"
	return gateway
}

func (g *fakeGateway) setAutoAck(ack bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.autoAck = ack
}

func (g *fakeGateway) getAutoAck() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.autoAck
}

func (g *fakeGateway) handle(ws *websocket.Conn) {
	g.conns <- ws
	for {
		var raw []byte
		if err := websocket.Message.Receive(ws, &raw); err != nil {
			return
		}
		var frame map[string]any
		if err := json.Unmarshal(raw, &frame); err != nil {
			continue
		}
		g.frames <- frame
		if g.getAutoAck() {
			if op, _ := frame["op"].(float64); op == float64(opHeartbeat) {
				_ = websocket.Message.Send(ws, []byte(`{"op":11}`))
			}
		}
	}
}

func (g *fakeGateway) nextConn(timeout time.Duration) *websocket.Conn {
	g.t.Helper()
	select {
	case ws := <-g.conns:
		return ws
	case <-time.After(timeout):
		g.t.Fatal("timed out waiting for gateway connection")
		return nil
	}
}

func (g *fakeGateway) nextFrame(timeout time.Duration) map[string]any {
	g.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			g.t.Fatal("timed out waiting for client frame")
		}
		select {
		case frame := <-g.frames:
			if op, _ := frame["op"].(float64); op == float64(opHeartbeat) {
				continue
			}
			return frame
		case <-time.After(remaining):
			g.t.Fatal("timed out waiting for client frame")
			return nil
		}
	}
}

func (g *fakeGateway) send(ws *websocket.Conn, op int, data any, seq *int64, typ string) {
	g.t.Helper()
	raw, err := json.Marshal(map[string]any{"op": op, "d": data, "s": seq, "t": typ})
	if err != nil {
		g.t.Fatalf("marshal frame: %v", err)
	}
	if err := websocket.Message.Send(ws, raw); err != nil {
		g.t.Fatalf("send frame: %v", err)
	}
}

func testAdapterConfig() config.Discord {
	return config.Discord{
		Enabled:            true,
		Instance:           "owner",
		BotTokenRef:        "env://AURA_DISCORD_BOT_TOKEN",
		AllowedUserIDs:     []string{"111"},
		AllowedGuildIDs:    []string{"222"},
		AllowedChannelIDs:  []string{"333"},
		AcceptDMs:          true,
		MinEditInterval:    2000000000,
		MaxAttachmentBytes: 20971520,
	}
}

func startTestAdapter(t *testing.T, gateway *fakeGateway, sink *memorySink, resumes *memoryResumeStore, logs *logCapture) (*Adapter, context.CancelFunc, chan error) {
	t.Helper()
	t.Setenv("AURA_DISCORD_BOT_TOKEN", testToken)
	logger := slog.New(slog.NewTextHandler(logs, nil))
	discordCfg := testAdapterConfig()
	adapter, err := New(&discordCfg, resumes, logger)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	adapter.retryBase = 5 * time.Millisecond
	adapter.retryCap = 50 * time.Millisecond
	adapter.invalidDelay = 5 * time.Millisecond
	adapter.restBase = gateway.self.URL
	adapter.dial = func(ctx context.Context, url string) (*websocket.Conn, error) {
		type dialResult struct {
			ws  *websocket.Conn
			err error
		}
		result := make(chan dialResult, 1)
		go func() {
			ws, err := websocket.Dial(gateway.url, "", "http://localhost/")
			result <- dialResult{ws, err}
		}()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case outcome := <-result:
			return outcome.ws, outcome.err
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- adapter.Start(ctx, sink) }()
	return adapter, cancel, done
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func helloData(intervalMs int64) map[string]any {
	return map[string]any{"heartbeat_interval": intervalMs}
}

func readyData(session, user string) map[string]any {
	return map[string]any{
		"session_id":         session,
		"resume_gateway_url": "",
		"user":               map[string]any{"id": user, "bot": true},
	}
}

func messageData(id, channel, guild, author, content string, mentions []string) map[string]any {
	mentionList := make([]any, 0, len(mentions))
	for _, mention := range mentions {
		mentionList = append(mentionList, map[string]any{"id": mention})
	}
	return map[string]any{
		"id":         id,
		"channel_id": channel,
		"guild_id":   guild,
		"author":     map[string]any{"id": author, "bot": false},
		"content":    content,
		"mentions":   mentionList,
	}
}

func seqPtr(seq int64) *int64 { return &seq }

func frameOp(t *testing.T, frame map[string]any) int {
	t.Helper()
	op, ok := frame["op"].(float64)
	if !ok {
		t.Fatalf("client frame has no op: %v", frame)
	}
	return int(op)
}
