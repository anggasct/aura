package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"

	"github.com/anggasct/aura/internal/config"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
)

const (
	testWebhookSecretEnv  = "AURA_TEST_TELEGRAM_WEBHOOK_SECRET"
	testWebhookSecret     = "hook_secret-probe-Q7x2"
	testPublicBase        = "https://aura.example.test"
	testWebhookURL        = testPublicBase + "/telegram/pocket"
	testWebhookConns      = 3
	testWebhookBodyBytes  = 4096
	testWrongSecretProbe  = "wrong-secret-probe-5d2e"
	testWebhookRoute      = "/telegram/pocket"
	testWebhookJSONHeader = "application/json"
)

type webhookReply struct {
	url              string
	lastErrorDate    int64
	lastErrorMessage string
}

func (f *fakeBotAPI) setHook(reply webhookReply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hook = reply
}

func (f *fakeBotAPI) serveWebhookCall(w http.ResponseWriter, method string) {
	if method == "setWebhook" {
		writeOK(w, true)
		return
	}
	f.mu.Lock()
	hook := f.hook
	f.mu.Unlock()
	writeOK(w, map[string]any{
		"url":                  hook.url,
		"pending_update_count": 0,
		"last_error_date":      hook.lastErrorDate,
		"last_error_message":   hook.lastErrorMessage,
	})
}

type countingLedger struct {
	mu   sync.Mutex
	asks int
}

func (l *countingLedger) Claimed(context.Context, string, string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.asks++
	return false, nil
}

func (l *countingLedger) asked() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.asks
}

func (s *memorySink) totalCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, n := range s.calls {
		total += n
	}
	return total
}

type gateSink struct {
	entered chan string
	release chan struct{}
	mu      sync.Mutex
	active  int
	peak    int
}

func newGateSink() *gateSink {
	return &gateSink{entered: make(chan string, 64), release: make(chan struct{})}
}

func (g *gateSink) Accept(ctx context.Context, env *runtimeingress.IngressEnvelope) (runtimeingress.TurnRef, error) {
	g.mu.Lock()
	g.active++
	g.peak = max(g.peak, g.active)
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.active--
		g.mu.Unlock()
	}()
	g.entered <- env.ExternalID
	select {
	case <-g.release:
		return runtimeingress.TurnRef{TurnID: "turn-" + env.ExternalID, SessionID: env.ConversationID}, nil
	case <-ctx.Done():
		return runtimeingress.TurnRef{}, ctx.Err()
	}
}

func (g *gateSink) highWater() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.peak
}

type webhookRig struct {
	h       *harness
	adapter *Adapter
	peer    *Adapter
	handler *WebhookHandler
	http    *httptest.Server
	ledger  *countingLedger
}

type webhookResult struct {
	status int
	header http.Header
	body   string
}

func newWebhookRig(t *testing.T, configure func(*config.Telegram, *config.TelegramAccount)) *webhookRig {
	t.Helper()
	h := newHarness(t)
	t.Setenv(testWebhookSecretEnv, testWebhookSecret)
	adapter := h.adapterWith(t, nil, func(cfg *config.Telegram, account *config.TelegramAccount) {
		account.Mode = config.TelegramModeWebhook
		account.WebhookSecretRef = "env://" + testWebhookSecretEnv
		cfg.Webhook = config.TelegramWebhook{
			ListenAddress:  config.TelegramWebhookListenAddress,
			PublicBaseURL:  testPublicBase,
			MaxBodyBytes:   testWebhookBodyBytes,
			MaxConnections: testWebhookConns,
		}
		if configure != nil {
			configure(cfg, account)
		}
	})
	peer := h.adapterWith(t, nil, func(_ *config.Telegram, account *config.TelegramAccount) {
		account.Name = "lounge"
	})
	handler, err := NewWebhookHandler([]*Adapter{adapter, peer})
	if err != nil {
		t.Fatalf("NewWebhookHandler: %v", err)
	}
	ledger := &countingLedger{}
	adapter.SetUpdateLedger(ledger)
	rig := &webhookRig{h: h, adapter: adapter, peer: peer, handler: handler, ledger: ledger}
	rig.http = httptest.NewServer(handler)
	t.Cleanup(rig.http.Close)
	return rig
}

func (r *webhookRig) start(t *testing.T, sink runtimeingress.IngressSink) func() {
	t.Helper()
	stop := startAdapter(t, r.adapter, sink)
	eventually(t, "webhook registration", func() bool {
		return r.adapter.Health(t.Context()).Detail == config.TelegramModeWebhook
	})
	return stop
}

func (r *webhookRig) post(t *testing.T, path string, body []byte, mutate func(*http.Request)) webhookResult {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, r.http.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Errorf("NewRequest: %v", err)
		return webhookResult{status: -1}
	}
	req.Header.Set("Content-Type", testWebhookJSONHeader)
	req.Header.Set(webhookAuthHeader, testWebhookSecret)
	if mutate != nil {
		mutate(req)
	}
	return r.do(t, req)
}

func (r *webhookRig) do(t *testing.T, req *http.Request) webhookResult {
	t.Helper()
	resp, err := r.http.Client().Do(req)
	if err != nil {
		t.Errorf("webhook request: %v", err)
		return webhookResult{status: -1}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Errorf("read response: %v", err)
		return webhookResult{status: -1}
	}
	return webhookResult{status: resp.StatusCode, header: resp.Header, body: string(raw)}
}

func (r *webhookRig) deliver(t *testing.T, item update) webhookResult {
	t.Helper()
	return r.post(t, testWebhookRoute, mustJSON(t, item), nil)
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func goroutinesIn(marker string) int {
	var dump bytes.Buffer
	if err := pprof.Lookup("goroutine").WriteTo(&dump, 2); err != nil {
		return -1
	}
	return strings.Count(dump.String(), marker)
}
