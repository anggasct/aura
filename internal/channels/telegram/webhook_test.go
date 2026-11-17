package telegram

import (
	"cmp"
	"context"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
)

func TestWebhookRegistersWithSecretAndNeverPolls(t *testing.T) {
	rig := newWebhookRig(t, nil)
	sink := newMemorySink()
	stop := rig.start(t, sink)
	calls := rig.h.api.bodies("setWebhook")
	if len(calls) != 1 {
		t.Fatalf("setWebhook calls = %d, want 1", len(calls))
	}
	body := calls[0]
	if body["url"] != testWebhookURL {
		t.Errorf("url = %v, want %s", body["url"], testWebhookURL)
	}
	if body["secret_token"] != testWebhookSecret {
		t.Errorf("secret_token does not carry the resolved secret")
	}
	if allowed, _ := body["allowed_updates"].([]any); len(allowed) != 2 || allowed[0] != "message" || allowed[1] != "callback_query" {
		t.Errorf("allowed_updates = %v, want message and callback_query", body["allowed_updates"])
	}
	if conns, _ := body["max_connections"].(float64); int(conns) != testWebhookConns {
		t.Errorf("max_connections = %v, want %d", body["max_connections"], testWebhookConns)
	}
	if got := rig.adapter.Health(t.Context()); got.Status != runtimechannelhost.ChannelHealthy || got.Detail != "webhook" {
		t.Errorf("health = %+v, want healthy webhook", got)
	}
	result := rig.deliver(t, privateText(10, ownerUserID, "hello"))
	if result.status != http.StatusOK || result.body != "" {
		t.Fatalf("delivery = %d %q, want 200 with an empty body", result.status, result.body)
	}
	accepted := sink.accepted()
	if len(accepted) != 1 || accepted[0].ExternalID != "update:10" || accepted[0].Parts[0].Text != "hello" {
		t.Fatalf("accepted = %+v, want the delivered update through the polling path", accepted)
	}
	stop()
	if n := rig.h.api.count("getUpdates"); n != 0 {
		t.Errorf("getUpdates calls = %d, want 0 in webhook mode", n)
	}
	if n := rig.h.api.count("deleteWebhook"); n != 0 {
		t.Errorf("deleteWebhook calls = %d, want 0 on shutdown", n)
	}
}

func TestWebhookRegistrationRetriesUntilTelegramAccepts(t *testing.T) {
	rig := newWebhookRig(t, nil)
	release := make(chan struct{})
	rig.h.api.failNextOn("setWebhook", func(w http.ResponseWriter) bool {
		<-release
		writeFailure(w, http.StatusInternalServerError, "unavailable", 0)
		return true
	})
	rig.h.api.failNextOn("setWebhook", apiFailure(http.StatusInternalServerError, 0))
	stop := startAdapter(t, rig.adapter, newMemorySink())
	defer stop()
	eventually(t, "first registration attempt", func() bool { return rig.h.api.count("setWebhook") == 1 })
	if got := rig.adapter.Health(t.Context()); got.Status != runtimechannelhost.ChannelDown {
		t.Errorf("health while unregistered = %+v, want down", got)
	}
	if findings := rig.adapter.Check(t.Context()); len(findings) != 0 {
		t.Errorf("findings while unregistered = %+v, want none", findings)
	}
	if n := rig.h.api.count("getWebhookInfo"); n != 0 {
		t.Errorf("getWebhookInfo calls = %d before registration, want 0", n)
	}
	close(release)
	eventually(t, "registration", func() bool {
		return rig.adapter.Health(t.Context()).Status == runtimechannelhost.ChannelHealthy
	})
	if n := rig.h.api.count("setWebhook"); n != 3 {
		t.Errorf("setWebhook calls = %d, want 3 (two failures then success)", n)
	}
	if current := rig.adapter.currentFault(); current != faultNone {
		t.Errorf("fault after registration = %q, want none", current)
	}
}

func TestWebhookRejectsInvalidSecretWithoutRegistering(t *testing.T) {
	cases := map[string]string{
		"empty":         "",
		"too long":      strings.Repeat("a", 257),
		"space":         "has space",
		"slash":         "a/b",
		"non ascii":     "café",
		"punctuation":   "a.b",
		"only newlines": "\n\n",
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			rig := newWebhookRig(t, nil)
			t.Setenv(testWebhookSecretEnv, value)
			stop := startAdapter(t, rig.adapter, newMemorySink())
			defer stop()
			eventually(t, "secret fault", func() bool { return rig.adapter.currentFault() == faultWebhookSecretInvalid })
			if n := rig.h.api.count("setWebhook"); n != 0 {
				t.Errorf("setWebhook calls = %d, want 0 with an invalid secret", n)
			}
			if got := rig.adapter.Health(t.Context()); got.Status != runtimechannelhost.ChannelDown {
				t.Errorf("health = %+v, want down", got)
			}
			findings := rig.adapter.Check(t.Context())
			if len(findings) != 1 || findings[0].Code != "telegram_webhook_secret_invalid" || findings[0].Remediation != RemediationReviewBot {
				t.Errorf("findings = %+v, want the secret finding", findings)
			}
			result := rig.deliver(t, privateText(10, ownerUserID, "hello"))
			if result.status != http.StatusServiceUnavailable {
				t.Errorf("delivery = %d, want 503 while the account is not serving", result.status)
			}
		})
	}
}

func TestWebhookRejectsBeforeAnyWork(t *testing.T) {
	valid := mustJSON(t, privateText(10, ownerUserID, "hello"))
	media := mustJSON(t, update{UpdateID: 11, Message: &message{
		MessageID: 110,
		From:      &user{ID: ownerUserID},
		Chat:      chat{ID: ownerUserID, Type: chatTypePrivate},
		Photo:     []mediaFile{{FileID: "photo-1", FileSize: 100}},
	}})
	oversized := []byte(`{"update_id":12,"pad":"` + strings.Repeat("x", testWebhookBodyBytes) + `"}`)
	cases := []struct {
		name   string
		method string
		path   string
		body   []byte
		mutate func(*http.Request)
		want   int
	}{
		{name: "unknown account route", path: "/telegram/other", body: valid, want: http.StatusNotFound},
		{name: "polling account route", path: "/telegram/lounge", body: valid, want: http.StatusNotFound},
		{name: "trailing slash", path: "/telegram/pocket/", body: valid, want: http.StatusNotFound},
		{name: "root", path: "/", body: valid, want: http.StatusNotFound},
		{name: "get", method: http.MethodGet, body: valid, want: http.StatusMethodNotAllowed},
		{name: "put", method: http.MethodPut, body: valid, want: http.StatusMethodNotAllowed},
		{name: "text content type", body: valid, mutate: func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, want: http.StatusUnsupportedMediaType},
		{name: "missing content type", body: valid, mutate: func(r *http.Request) { r.Header.Del("Content-Type") }, want: http.StatusUnsupportedMediaType},
		{name: "form content type", body: valid, mutate: func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") }, want: http.StatusUnsupportedMediaType},
		{name: "missing secret", body: valid, mutate: func(r *http.Request) { r.Header.Del(webhookAuthHeader) }, want: http.StatusUnauthorized},
		{name: "wrong secret", body: valid, mutate: func(r *http.Request) { r.Header.Set(webhookAuthHeader, testWrongSecretProbe) }, want: http.StatusUnauthorized},
		{name: "secret prefix", body: valid, mutate: func(r *http.Request) { r.Header.Set(webhookAuthHeader, testWebhookSecret[:8]) }, want: http.StatusUnauthorized},
		{name: "secret with suffix", body: valid, mutate: func(r *http.Request) { r.Header.Set(webhookAuthHeader, testWebhookSecret+"x") }, want: http.StatusUnauthorized},
		{name: "empty secret", body: valid, mutate: func(r *http.Request) { r.Header.Set(webhookAuthHeader, "") }, want: http.StatusUnauthorized},
		{name: "repeated secret header", body: valid, mutate: func(r *http.Request) { r.Header.Add(webhookAuthHeader, testWebhookSecret) }, want: http.StatusUnauthorized},
		{name: "oversized with length", body: oversized, want: http.StatusRequestEntityTooLarge},
		{name: "oversized chunked", body: oversized, mutate: func(r *http.Request) {
			r.ContentLength = -1
			r.Body = io.NopCloser(strings.NewReader(string(oversized)))
		}, want: http.StatusRequestEntityTooLarge},
		{name: "empty body", body: nil, want: http.StatusBadRequest},
		{name: "malformed json", body: []byte(`{"update_id":`), want: http.StatusBadRequest},
		{name: "not an object", body: []byte(`[1,2]`), want: http.StatusBadRequest},
		{name: "trailing data", body: append(slices.Clone(valid), []byte(` {"update_id":99}`)...), want: http.StatusBadRequest},
		{name: "missing update id", body: []byte(`{"message":{"message_id":1}}`), want: http.StatusBadRequest},
		{name: "negative update id", body: []byte(`{"update_id":-4}`), want: http.StatusBadRequest},
		{name: "mistyped field", body: []byte(`{"update_id":13,"message":{"message_id":"one"}}`), want: http.StatusBadRequest},
	}
	rig := newWebhookRig(t, nil)
	sink := newMemorySink()
	stop := rig.start(t, sink)
	defer stop()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := cmp.Or(tc.path, testWebhookRoute)
			req, err := http.NewRequestWithContext(t.Context(), cmp.Or(tc.method, http.MethodPost), rig.http.URL+path, strings.NewReader(string(tc.body)))
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			req.Header.Set("Content-Type", testWebhookJSONHeader)
			req.Header.Set(webhookAuthHeader, testWebhookSecret)
			if tc.mutate != nil {
				tc.mutate(req)
			}
			result := rig.do(t, req)
			if result.status != tc.want {
				t.Fatalf("status = %d, want %d", result.status, tc.want)
			}
			if result.body != "" {
				t.Errorf("rejection carries a body: %q", result.body)
			}
			if tc.want == http.StatusMethodNotAllowed && result.header.Get("Allow") != http.MethodPost {
				t.Errorf("Allow = %q, want POST", result.header.Get("Allow"))
			}
			if total := sink.totalCalls(); total != 0 {
				t.Errorf("runtime was reached %d times by a rejected request", total)
			}
			if asks := rig.ledger.asked(); asks != 0 {
				t.Errorf("dedupe ledger consulted %d times by a rejected request", asks)
			}
			for _, method := range []string{"getFile", "sendMessage", "getUpdates"} {
				if n := rig.h.api.count(method); n != 0 {
					t.Errorf("%s called %d times by a rejected request", method, n)
				}
			}
			logs := rig.h.logs.String()
			for _, leaked := range []string{testWebhookSecret, testWrongSecretProbe, "hello"} {
				if strings.Contains(logs, leaked) {
					t.Errorf("logs carry %q: %s", leaked, logs)
				}
			}
		})
	}
	t.Run("media is not fetched for a rejected request", func(t *testing.T) {
		result := rig.post(t, testWebhookRoute, media, func(r *http.Request) { r.Header.Set(webhookAuthHeader, testWrongSecretProbe) })
		if result.status != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", result.status)
		}
		if n := rig.h.api.count("getFile"); n != 0 || sink.totalCalls() != 0 || rig.ledger.asked() != 0 {
			t.Errorf("media path touched: getFile=%d sink=%d ledger=%d", n, sink.totalCalls(), rig.ledger.asked())
		}
	})
}

func TestWebhookClassifiesOutcomes(t *testing.T) {
	rig := newWebhookRig(t, nil)
	sink := newMemorySink()
	sink.failOnce("update:21", overloaded())
	sink.failOnce("update:22", fault.New(turn.ErrorCodeInvalidArgument, "rejected"))
	stop := rig.start(t, sink)
	defer stop()
	callback := update{UpdateID: 24, CallbackQuery: &callbackQuery{
		ID:      "cb-probe",
		From:    &user{ID: ownerUserID},
		Message: &message{MessageID: 5, Chat: chat{ID: ownerUserID, Type: chatTypePrivate}},
		Data:    "not-a-card",
	}}
	cases := []struct {
		name string
		item update
		want int
	}{
		{name: "accepted", item: privateText(20, ownerUserID, "one"), want: http.StatusOK},
		{name: "transient runtime failure", item: privateText(21, ownerUserID, "two"), want: http.StatusInternalServerError},
		{name: "retry of the failed update", item: privateText(21, ownerUserID, "two"), want: http.StatusOK},
		{name: "permanent runtime rejection", item: privateText(22, ownerUserID, "three"), want: http.StatusOK},
		{name: "unknown sender", item: privateText(23, strangerID, "let me in"), want: http.StatusOK},
		{name: "unmatched callback", item: callback, want: http.StatusOK},
		{name: "update without a message", item: update{UpdateID: 25}, want: http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := rig.deliver(t, tc.item)
			if result.status != tc.want {
				t.Fatalf("status = %d, want %d", result.status, tc.want)
			}
			if result.body != "" {
				t.Errorf("body = %q, want empty", result.body)
			}
		})
	}
	if calls, turns := sink.stats("update:21"); calls != 2 || turns != 1 {
		t.Errorf("update:21 calls=%d turns=%d, want 2 calls and 1 turn", calls, turns)
	}
	if _, turns := sink.stats("update:23"); turns != 0 {
		t.Errorf("unknown sender created %d turns", turns)
	}
	if denied := rig.h.denied(); len(denied) != 1 || denied[0] != DeniedReasonIdentity {
		t.Errorf("denials = %v, want one identity denial", denied)
	}
	if n := rig.h.api.count("answerCallbackQuery"); n != 1 {
		t.Errorf("answerCallbackQuery calls = %d, want 1", n)
	}
}

func TestWebhookIntakeFailureSurfacesInHealthUntilRecovery(t *testing.T) {
	rig := newWebhookRig(t, nil)
	sink := newMemorySink()
	sink.failOnce("update:30", overloaded())
	stop := rig.start(t, sink)
	defer stop()
	if result := rig.deliver(t, privateText(30, ownerUserID, "one")); result.status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", result.status)
	}
	if got := rig.adapter.Health(t.Context()); got.Status != runtimechannelhost.ChannelDown {
		t.Errorf("health after a failed intake = %+v, want down", got)
	}
	findings := rig.adapter.Check(t.Context())
	if len(findings) != 1 || findings[0].Code != faultIntake {
		t.Fatalf("findings = %+v, want telegram_intake_failed", findings)
	}
	if result := rig.deliver(t, privateText(30, ownerUserID, "one")); result.status != http.StatusOK {
		t.Fatalf("redelivery status = %d, want 200", result.status)
	}
	if got := rig.adapter.Health(t.Context()); got.Status != runtimechannelhost.ChannelHealthy || got.Detail != "webhook" {
		t.Errorf("health after recovery = %+v, want healthy webhook", got)
	}
}

func TestWebhookConcurrentDuplicatesCreateOneTurnPerUpdate(t *testing.T) {
	const updates, copies = 12, 5
	rig := newWebhookRig(t, func(cfg *config.Telegram, _ *config.TelegramAccount) {
		cfg.Webhook.MaxConnections = updates * copies
	})
	sink := newMemorySink()
	stop := rig.start(t, sink)
	defer stop()
	begin := make(chan struct{})
	statuses := make(chan int, updates*copies)
	var deliveries sync.WaitGroup
	for id := range updates {
		item := privateText(int64(100+id), ownerUserID, "message "+strconv.Itoa(id))
		for range copies {
			deliveries.Go(func() {
				<-begin
				statuses <- rig.deliver(t, item).status
			})
		}
	}
	close(begin)
	deliveries.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusOK {
			t.Errorf("status = %d, want 200 for every delivery and duplicate", status)
		}
	}
	for id := range updates {
		key := "update:" + strconv.Itoa(100+id)
		if calls, turns := sink.stats(key); turns != 1 || calls != copies {
			t.Errorf("%s calls=%d turns=%d, want %d calls and exactly one turn", key, calls, turns, copies)
		}
	}
	if got := len(sink.accepted()); got != updates {
		t.Errorf("accepted %d envelopes, want %d", got, updates)
	}
}

func TestWebhookNeverExceedsMaxConnections(t *testing.T) {
	const limit = 2
	rig := newWebhookRig(t, func(cfg *config.Telegram, _ *config.TelegramAccount) { cfg.Webhook.MaxConnections = limit })
	gate := newGateSink()
	stop := rig.start(t, gate)
	defer stop()
	results := make(chan int, limit)
	var held sync.WaitGroup
	for id := range limit {
		held.Go(func() { results <- rig.deliver(t, privateText(int64(200+id), ownerUserID, "held")).status })
	}
	for range limit {
		select {
		case <-gate.entered:
		case <-time.After(20 * time.Second):
			t.Fatal("deliveries did not reach the runtime")
		}
	}
	over := rig.deliver(t, privateText(210, ownerUserID, "over the cap"))
	if over.status != http.StatusServiceUnavailable || over.header.Get("Retry-After") == "" {
		t.Fatalf("over-cap delivery = %d retry-after %q, want 503 with Retry-After", over.status, over.header.Get("Retry-After"))
	}
	close(gate.release)
	held.Wait()
	close(results)
	for status := range results {
		if status != http.StatusOK {
			t.Errorf("held delivery status = %d, want 200", status)
		}
	}
	if again := rig.deliver(t, privateText(210, ownerUserID, "over the cap")); again.status != http.StatusOK {
		t.Errorf("delivery after release = %d, want 200", again.status)
	}
	if peak := gate.highWater(); peak > limit {
		t.Errorf("peak concurrent deliveries = %d, want at most %d", peak, limit)
	}
}

func TestWebhookHealthFindingsFollowGetWebhookInfo(t *testing.T) {
	cases := []struct {
		name string
		info webhookReply
		fail func(*fakeBotAPI)
		want string
	}{
		{name: "registered and quiet", info: webhookReply{url: testWebhookURL}},
		{name: "old error", info: webhookReply{url: testWebhookURL, lastErrorDate: time.Now().Add(-time.Hour).Unix(), lastErrorMessage: "old error"}},
		{name: "different url", info: webhookReply{url: "https://elsewhere.example.test/telegram/pocket"}, want: "telegram_webhook_mismatch"},
		{name: "webhook removed", info: webhookReply{}, want: "telegram_webhook_mismatch"},
		{name: "recent error", info: webhookReply{url: testWebhookURL, lastErrorDate: time.Now().Add(-time.Minute).Unix(), lastErrorMessage: "Wrong response from the webhook: 502 error-text-probe"}, want: "telegram_webhook_failing"},
		{name: "mismatch wins over a recent error", info: webhookReply{url: "https://elsewhere.example.test", lastErrorDate: time.Now().Unix()}, want: "telegram_webhook_mismatch"},
		{name: "token rejected", info: webhookReply{url: testWebhookURL}, fail: func(api *fakeBotAPI) { api.failNextOn("getWebhookInfo", apiFailure(http.StatusUnauthorized, 0)) }, want: "telegram_unauthorized"},
		{name: "api down", info: webhookReply{url: testWebhookURL}, fail: func(api *fakeBotAPI) { api.failNextOn("getWebhookInfo", apiFailure(http.StatusBadGateway, 0)) }, want: "telegram_unreachable"},
	}
	rig := newWebhookRig(t, nil)
	stop := rig.start(t, newMemorySink())
	defer stop()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig.h.api.setHook(tc.info)
			if tc.fail != nil {
				tc.fail(rig.h.api)
			}
			findings := rig.adapter.Check(t.Context())
			if tc.want == "" {
				if len(findings) != 0 {
					t.Fatalf("findings = %+v, want none", findings)
				}
				return
			}
			if len(findings) != 1 {
				t.Fatalf("findings = %+v, want exactly %s", findings, tc.want)
			}
			finding := findings[0]
			if finding.Code != tc.want || finding.Component != "telegram" || finding.Status != health.StatusDegraded || finding.Remediation != RemediationReviewBot {
				t.Errorf("finding = %+v, want degraded %s", finding, tc.want)
			}
			if strings.Contains(finding.Detail, "error-text-probe") || strings.Contains(finding.Detail, "elsewhere") {
				t.Errorf("finding detail echoes telegram data: %q", finding.Detail)
			}
		})
	}
}

func TestWebhookSecretNeverSurfaces(t *testing.T) {
	rig := newWebhookRig(t, nil)
	rig.h.api.failNextOn("setWebhook", describedFailure(http.StatusBadRequest, "bad webhook "+testWebhookSecret, 0))
	stop := rig.start(t, newMemorySink())
	defer stop()
	rig.deliver(t, privateText(40, ownerUserID, "hello"))
	rig.post(t, testWebhookRoute, mustJSON(t, privateText(41, ownerUserID, "hello")), func(r *http.Request) {
		r.Header.Set(webhookAuthHeader, testWrongSecretProbe)
	})
	rig.rejectedRequests(t)
	surfaces := []string{rig.h.logs.String(), rig.adapter.Health(t.Context()).Detail}
	for _, finding := range rig.adapter.Check(t.Context()) {
		surfaces = append(surfaces, finding.Detail)
	}
	for _, surface := range surfaces {
		for _, leaked := range []string{testWebhookSecret, testWrongSecretProbe} {
			if strings.Contains(surface, leaked) {
				t.Errorf("secret %q surfaced in %q", leaked, surface)
			}
		}
	}
}

func (r *webhookRig) rejectedRequests(t *testing.T) {
	t.Helper()
	for _, mutate := range []func(*http.Request){
		func(req *http.Request) { req.Header.Set("Content-Type", "text/plain") },
		func(req *http.Request) { req.Header.Del(webhookAuthHeader) },
	} {
		r.post(t, testWebhookRoute, []byte(`{}`), mutate)
	}
}

func TestNewRejectsIncompleteWebhookAccounts(t *testing.T) {
	cases := map[string]func(*config.Telegram, *config.TelegramAccount){
		"unknown mode":        func(_ *config.Telegram, a *config.TelegramAccount) { a.Mode = "carrier-pigeon" },
		"missing secret ref":  func(_ *config.Telegram, a *config.TelegramAccount) { a.WebhookSecretRef = "" },
		"inline secret":       func(_ *config.Telegram, a *config.TelegramAccount) { a.WebhookSecretRef = "plain-secret" },
		"missing public url":  func(c *config.Telegram, _ *config.TelegramAccount) { c.Webhook.PublicBaseURL = "" },
		"zero body bound":     func(c *config.Telegram, _ *config.TelegramAccount) { c.Webhook.MaxBodyBytes = 0 },
		"zero connection cap": func(c *config.Telegram, _ *config.TelegramAccount) { c.Webhook.MaxConnections = 0 },
	}
	h := newHarness(t)
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := &config.Telegram{
				Polling: config.TelegramPolling{Timeout: config.Duration(time.Second), MaxBackoff: config.Duration(time.Second)},
				Webhook: config.TelegramWebhook{PublicBaseURL: testPublicBase, MaxBodyBytes: 1024, MaxConnections: 2},
			}
			account := &config.TelegramAccount{Name: "pocket", BotTokenRef: "env://" + testTokenEnv, Mode: config.TelegramModeWebhook, WebhookSecretRef: "env://" + testWebhookSecretEnv}
			mutate(cfg, account)
			_, err := New(cfg, account, h.cursors, h.effects, h.sessions, nil)
			if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
				t.Fatalf("New error = %v, want invalid_argument", err)
			}
		})
	}
}

func TestNewWebhookHandlerRequiresWebhookAccounts(t *testing.T) {
	h := newHarness(t)
	polling := h.adapter(t, nil)
	duplicate := newWebhookRig(t, nil).adapter
	cases := map[string][]*Adapter{
		"none":            {},
		"only polling":    {polling},
		"nil adapter":     {nil},
		"duplicate route": {duplicate, duplicate},
	}
	for name, adapters := range cases {
		t.Run(name, func(t *testing.T) {
			handler, err := NewWebhookHandler(adapters)
			if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument || handler != nil {
				t.Fatalf("NewWebhookHandler = %v, %v; want invalid_argument and no handler", handler, err)
			}
		})
	}
}

func TestNewWebhookServerValidatesArguments(t *testing.T) {
	handler := newWebhookRig(t, nil).handler
	cases := map[string]struct {
		handler *WebhookHandler
		listen  string
		grace   time.Duration
	}{
		"no handler": {listen: "127.0.0.1:0", grace: time.Second},
		"no address": {handler: handler, grace: time.Second},
		"no grace":   {handler: handler, listen: "127.0.0.1:0"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			server, err := NewWebhookServer(tc.handler, tc.listen, tc.grace)
			if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument || server != nil {
				t.Fatalf("NewWebhookServer = %v, %v; want invalid_argument and no server", server, err)
			}
		})
	}
}

type servedWebhook struct {
	url    string
	cancel context.CancelFunc
	done   chan error
	client *http.Client
}

func serveWebhookRig(t *testing.T, rig *webhookRig, grace time.Duration) *servedWebhook {
	t.Helper()
	server, err := NewWebhookServer(rig.handler, "127.0.0.1:0", grace)
	if err != nil {
		t.Fatalf("NewWebhookServer: %v", err)
	}
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	transport := &http.Transport{DisableKeepAlives: true}
	t.Cleanup(transport.CloseIdleConnections)
	return &servedWebhook{
		url:    "http://" + listener.Addr().String() + testWebhookRoute,
		cancel: cancel,
		done:   done,
		client: &http.Client{Transport: transport},
	}
}

func (s *servedWebhook) send(t *testing.T, body []byte) <-chan int {
	t.Helper()
	status := make(chan int, 1)
	go func() {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.url, strings.NewReader(string(body)))
		if err != nil {
			status <- -1
			return
		}
		req.Header.Set("Content-Type", testWebhookJSONHeader)
		req.Header.Set(webhookAuthHeader, testWebhookSecret)
		resp, err := s.client.Do(req)
		if err != nil {
			status <- -1
			return
		}
		_ = resp.Body.Close()
		status <- resp.StatusCode
	}()
	return status
}

func awaitStatus(t *testing.T, status <-chan int) int {
	t.Helper()
	select {
	case code := <-status:
		return code
	case <-time.After(20 * time.Second):
		t.Fatal("no webhook response within 20s")
		return 0
	}
}

func awaitServed(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(20 * time.Second):
		t.Fatal("webhook server did not stop within 20s")
		return nil
	}
}

func assertNoWebhookGoroutines(t *testing.T) {
	t.Helper()
	for _, marker := range []string{"(*Adapter).serveWebhook", "(*WebhookServer).Serve", "(*WebhookHandler).ServeHTTP"} {
		eventually(t, "no goroutine in "+marker, func() bool { return goroutinesIn(marker) == 0 })
	}
}

func TestWebhookServerDrainsInFlightDeliveryOnShutdown(t *testing.T) {
	rig := newWebhookRig(t, nil)
	gate := newGateSink()
	stopAdapter := rig.start(t, gate)
	defer stopAdapter()
	served := serveWebhookRig(t, rig, time.Minute)
	status := served.send(t, mustJSON(t, privateText(300, ownerUserID, "in flight")))
	select {
	case <-gate.entered:
	case <-time.After(20 * time.Second):
		t.Fatal("delivery did not reach the runtime")
	}
	served.cancel()
	close(gate.release)
	if code := awaitStatus(t, status); code != http.StatusOK {
		t.Errorf("in-flight delivery = %d, want 200 after drain", code)
	}
	if err := awaitServed(t, served.done); err != nil {
		t.Errorf("Serve() = %v, want a clean drain", err)
	}
	assertNoWebhookGoroutines(t)
	late := served.send(t, mustJSON(t, privateText(301, ownerUserID, "late")))
	if code := awaitStatus(t, late); code == http.StatusOK {
		t.Errorf("delivery after shutdown = %d, want it refused", code)
	}
}

func TestWebhookServerCancelsStuckDeliveryAfterGrace(t *testing.T) {
	rig := newWebhookRig(t, nil)
	gate := newGateSink()
	stopAdapter := rig.start(t, gate)
	defer stopAdapter()
	served := serveWebhookRig(t, rig, 50*time.Millisecond)
	status := served.send(t, mustJSON(t, privateText(310, ownerUserID, "stuck")))
	select {
	case <-gate.entered:
	case <-time.After(20 * time.Second):
		t.Fatal("delivery did not reach the runtime")
	}
	served.cancel()
	if code := awaitStatus(t, status); code != http.StatusInternalServerError {
		t.Errorf("cancelled delivery = %d, want 500 so Telegram retries", code)
	}
	err := awaitServed(t, served.done)
	if code, ok := CodeOf(err); !ok || code != ErrorCodeConnectionFailed {
		t.Errorf("Serve() = %v, want connection_failed after a forced drain", err)
	}
	if got := rig.adapter.Health(t.Context()); got.Status != runtimechannelhost.ChannelHealthy {
		t.Errorf("health after a cancelled delivery = %+v, want healthy", got)
	}
	assertNoWebhookGoroutines(t)
}

func TestWebhookServerBindFailureIsCoded(t *testing.T) {
	rig := newWebhookRig(t, nil)
	taken, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = taken.Close() }()
	server, err := NewWebhookServer(rig.handler, taken.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("NewWebhookServer: %v", err)
	}
	if server.Name() != "telegram-webhook" {
		t.Errorf("Name() = %q", server.Name())
	}
	err = server.Start(t.Context())
	if code, ok := CodeOf(err); !ok || code != ErrorCodeConnectionFailed {
		t.Fatalf("Start() = %v, want connection_failed", err)
	}
}

func TestWebhookServerStartServesUntilCancelled(t *testing.T) {
	rig := newWebhookRig(t, nil)
	probe, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	address := probe.Addr().String()
	_ = probe.Close()
	server, err := NewWebhookServer(rig.handler, address, time.Second)
	if err != nil {
		t.Fatalf("NewWebhookServer: %v", err)
	}
	stopAdapter := rig.start(t, newMemorySink())
	defer stopAdapter()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Start(ctx) }()
	served := &servedWebhook{url: "http://" + address + testWebhookRoute, client: &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}}
	eventually(t, "listener accepts deliveries", func() bool {
		return awaitStatus(t, served.send(t, mustJSON(t, privateText(320, ownerUserID, "hello")))) == http.StatusOK
	})
	cancel()
	if err := awaitServed(t, done); err != nil {
		t.Errorf("Start() = %v, want a clean stop", err)
	}
}

func TestPollingAdapterStillCancelsOutstandingPollOnShutdown(t *testing.T) {
	h := newHarness(t)
	h.api.hold = true
	stop := startAdapter(t, h.adapter(t, nil), newMemorySink())
	h.api.awaitPoll(t)
	stop()
	if n := h.api.count("setWebhook"); n != 0 {
		t.Errorf("setWebhook calls = %d for a polling account, want 0", n)
	}
	if n := h.api.count("deleteWebhook"); n != 0 {
		t.Errorf("deleteWebhook calls = %d for a polling account, want 0", n)
	}
}
