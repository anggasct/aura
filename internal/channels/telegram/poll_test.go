package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
)

func awaitCursor(t *testing.T, h *harness, want int64) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if next, _, _ := h.cursors.Load(t.Context(), "telegram:pocket"); next == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	next, _, _ := h.cursors.Load(t.Context(), "telegram:pocket")
	t.Fatalf("cursor = %d, want %d", next, want)
}

func TestPollAdmitsOnlyOwnerPrivateTextAndAdvancesCursor(t *testing.T) {
	h := newHarness(t)
	group := privateText(13, ownerUserID, "in a group")
	group.Message.Chat = chat{ID: -100500, Type: "supergroup"}
	fromBot := privateText(12, ownerUserID+1, "beep")
	fromBot.Message.From.IsBot = true
	h.api.queue(
		privateText(10, ownerUserID, "hello"),
		privateText(11, strangerID, "let me in"),
		fromBot,
		group,
		update{UpdateID: 14},
		privateText(15, ownerUserID, "   "),
	)
	sink := newMemorySink()
	stop := startAdapter(t, h.adapter(t, nil), sink)
	defer stop()
	if offset := h.api.awaitPoll(t); offset != 0 {
		t.Fatalf("first offset = %d, want 0 (no stored cursor)", offset)
	}
	if offset := h.api.awaitPoll(t); offset != 16 {
		t.Fatalf("second offset = %d, want 16", offset)
	}
	awaitCursor(t, h, 16)
	accepted := sink.accepted()
	if len(accepted) != 1 {
		t.Fatalf("accepted %d envelopes, want 1 (owner private text only)", len(accepted))
	}
	env := accepted[0]
	if env.Source != "telegram:pocket" || env.ExternalID != "update:10" || env.AgentID != testPersona {
		t.Errorf("envelope identity = %q %q %q", env.Source, env.ExternalID, env.AgentID)
	}
	if env.PrincipalID != principal.OwnerID || env.PrincipalClass != string(principal.ClassOwner) {
		t.Errorf("principal = %q %q, want the owner", env.PrincipalID, env.PrincipalClass)
	}
	if env.ConversationID != "main/telegram:pocket/chat:424242" {
		t.Errorf("conversation = %q", env.ConversationID)
	}
	if len(env.Parts) != 1 || env.Parts[0].Text != "hello" {
		t.Errorf("parts = %+v", env.Parts)
	}
	var reference replyReference
	if err := json.Unmarshal(env.ReplyContext, &reference); err != nil || reference.ChatID != ownerUserID || reference.MessageID != 100 {
		t.Errorf("reply context = %s (%v)", env.ReplyContext, err)
	}
	if _, err := h.sessions.sessions.Get(t.Context(), env.ConversationID); err != nil {
		t.Errorf("session was not ensured before acceptance: %v", err)
	}
	if denied := h.denied(); len(denied) != 1 || denied[0] != DeniedReasonIdentity {
		t.Errorf("denials = %v, want one identity denial", denied)
	}
	if sends := h.api.sends(); len(sends) != 0 {
		t.Errorf("rejected senders received %d replies", len(sends))
	}
	if logs := h.logs.String(); strings.Contains(logs, "let me in") || strings.Contains(logs, "999001") {
		t.Errorf("logs carry rejected content or identity: %s", logs)
	}
}

func TestPollRetriesTransientIntakeFailureWithoutSkipping(t *testing.T) {
	h := newHarness(t)
	h.api.queue(privateText(20, ownerUserID, "one"), privateText(21, ownerUserID, "two"), privateText(22, ownerUserID, "three"))
	h.api.queue(privateText(21, ownerUserID, "two"), privateText(22, ownerUserID, "three"))
	sink := newMemorySink()
	sink.failOnce("update:21", overloaded())
	adapter := h.adapter(t, nil)
	stop := startAdapter(t, adapter, sink)
	defer stop()
	h.api.awaitPoll(t)
	if offset := h.api.awaitPoll(t); offset != 21 {
		t.Fatalf("retry offset = %d, want 21 (the first unaccepted update)", offset)
	}
	awaitCursor(t, h, 23)
	for _, id := range []string{"update:20", "update:21", "update:22"} {
		if _, turns := sink.stats(id); turns != 1 {
			t.Errorf("%s turns = %d, want 1", id, turns)
		}
	}
	if calls, _ := sink.stats("update:21"); calls != 2 {
		t.Errorf("update:21 calls = %d, want 2", calls)
	}
}

func TestPollSkipsPermanentlyRejectedUpdate(t *testing.T) {
	h := newHarness(t)
	h.api.queue(privateText(30, ownerUserID, "bad"), privateText(31, ownerUserID, "good"))
	sink := newMemorySink()
	sink.failOnce("update:30", &turn.Error{Code: turn.ErrorCodeInvalidArgument, Detail: "rejected"})
	stop := startAdapter(t, h.adapter(t, nil), sink)
	defer stop()
	awaitCursor(t, h, 32)
	if _, turns := sink.stats("update:31"); turns != 1 {
		t.Errorf("update after a permanent reject was not accepted")
	}
}

func TestRestartReplaysUncommittedBatchThroughDedupe(t *testing.T) {
	h := newHarness(t)
	blocked := &failingCursors{CursorStore: h.cursors, failing: true}
	batch := []update{privateText(40, ownerUserID, "a"), privateText(41, ownerUserID, "b")}
	h.api.queue(batch...)
	sink := newMemorySink()
	stop := startAdapter(t, h.adapter(t, blocked), sink)
	h.api.awaitPoll(t)
	if offset := h.api.awaitPoll(t); offset != 0 {
		t.Fatalf("offset after a failed cursor write = %d, want 0", offset)
	}
	stop()
	if _, found, _ := h.cursors.Load(t.Context(), "telegram:pocket"); found {
		t.Fatal("cursor advanced although the write failed")
	}
	h.api.queue(batch...)
	stop = startAdapter(t, h.adapter(t, nil), sink)
	defer stop()
	awaitCursor(t, h, 42)
	for _, id := range []string{"update:40", "update:41"} {
		if calls, turns := sink.stats(id); calls != 2 || turns != 1 {
			t.Errorf("%s calls=%d turns=%d, want a replay absorbed into one turn", id, calls, turns)
		}
	}
}

func TestStartCancelsOutstandingLongPoll(t *testing.T) {
	h := newHarness(t)
	h.api.hold = true
	adapter := h.adapter(t, nil)
	stop := startAdapter(t, adapter, newMemorySink())
	h.api.awaitPoll(t)
	started := time.Now()
	stop()
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("shutdown took %v with a long poll outstanding", elapsed)
	}
}

func TestPollFaultsSurfaceInHealthWithoutTheToken(t *testing.T) {
	cases := map[string]struct {
		reply  func(w http.ResponseWriter) bool
		fault  string
		detail string
	}{
		"revoked token":   {reply: apiFailure(http.StatusUnauthorized, 0), fault: faultUnauthorized, detail: "rejected"},
		"second poller":   {reply: apiFailure(http.StatusConflict, 0), fault: faultPollConflict, detail: "another getUpdates poller"},
		"bot api outage":  {reply: apiFailure(http.StatusBadGateway, 0), fault: faultUnreachable, detail: "unreachable"},
		"flood wait":      {reply: apiFailure(http.StatusTooManyRequests, 1), fault: faultUnreachable, detail: "unreachable"},
		"undecodable 200": {reply: func(w http.ResponseWriter) bool { _, _ = w.Write([]byte("<html>")); return true }, fault: faultUnreachable, detail: "unreachable"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			for range 50 {
				h.api.failNext(tc.reply)
			}
			adapter := h.adapter(t, nil)
			stop := startAdapter(t, adapter, newMemorySink())
			defer stop()
			deadline := time.Now().Add(20 * time.Second)
			for adapter.currentFault() != tc.fault && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			status := adapter.Health(t.Context())
			if status.Status != runtimechannelhost.ChannelDown || !strings.Contains(status.Detail, tc.detail) {
				t.Fatalf("health = %+v, want down with %q", status, tc.detail)
			}
			findings := adapter.Check(t.Context())
			if len(findings) != 1 || findings[0].Code != tc.fault || findings[0].Remediation != RemediationReviewBot {
				t.Fatalf("findings = %+v, want %s", findings, tc.fault)
			}
			if strings.Contains(h.logs.String(), testToken) || strings.Contains(findings[0].Detail, testToken) {
				t.Fatal("bot token leaked into logs or findings")
			}
		})
	}
}

func TestPollRecoversToHealthy(t *testing.T) {
	h := newHarness(t)
	h.api.failNext(apiFailure(http.StatusBadGateway, 0))
	adapter := h.adapter(t, nil)
	stop := startAdapter(t, adapter, newMemorySink())
	defer stop()
	h.api.awaitPoll(t)
	h.api.awaitPoll(t)
	deadline := time.Now().Add(20 * time.Second)
	for adapter.Health(t.Context()).Status != runtimechannelhost.ChannelHealthy && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if status := adapter.Health(t.Context()); status.Status != runtimechannelhost.ChannelHealthy {
		t.Fatalf("health = %+v, want healthy after a successful poll", status)
	}
	if findings := adapter.Check(t.Context()); len(findings) != 0 {
		t.Fatalf("findings = %+v after recovery", findings)
	}
}

func TestMissingTokenReportsUnavailableWithoutPolling(t *testing.T) {
	h := newHarness(t)
	t.Setenv(testTokenEnv, "")
	adapter := h.adapter(t, nil)
	t.Setenv(testTokenEnv, "")
	stop := startAdapter(t, adapter, newMemorySink())
	defer stop()
	deadline := time.Now().Add(20 * time.Second)
	for adapter.currentFault() != faultBotRefUnresolved && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if adapter.currentFault() != faultBotRefUnresolved {
		t.Fatalf("fault = %q, want %q", adapter.currentFault(), faultBotRefUnresolved)
	}
	select {
	case <-h.api.polled:
		t.Fatal("polled without a token")
	default:
	}
}

func TestRetryDelayHonorsRetryAfterWithinTheCap(t *testing.T) {
	h := newHarness(t)
	adapter := h.adapter(t, nil)
	adapter.retryBase = 100 * time.Millisecond
	adapter.cfg.Polling.MaxBackoff = 2_000_000_000
	limited := func(after time.Duration) error {
		return &Error{Code: ErrorCodeBotAPIRejected, Detail: "bot api getUpdates failed", Cause: &APIError{Method: "getUpdates", Status: http.StatusTooManyRequests, RetryAfter: after}}
	}
	if got := adapter.retryDelay(0, limited(time.Second)); got != time.Second {
		t.Errorf("retry_after 1s → %v", got)
	}
	if got := adapter.retryDelay(0, limited(time.Hour)); got != 2*time.Second {
		t.Errorf("retry_after above the cap → %v, want the 2s cap", got)
	}
	plain := errors.New("transport")
	for attempt := range 12 {
		got := adapter.retryDelay(attempt, plain)
		ceiling := min(100*time.Millisecond<<min(attempt, 6), 2*time.Second)
		if got < ceiling/2 || got > ceiling {
			t.Errorf("attempt %d delay %v outside [%v, %v]", attempt, got, ceiling/2, ceiling)
		}
	}
}

func TestTransportErrorsNeverCarryTheToken(t *testing.T) {
	client := newBotClient()
	client.base = "http://127.0.0.1:1"
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, err := client.getUpdates(ctx, testToken, 0, time.Second)
	if err == nil || strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), "tok-secret") {
		t.Fatalf("transport error = %v, want one without the token", err)
	}
	client.base = "http://bad host"
	_, err = client.getUpdates(ctx, testToken, 0, time.Second)
	if err == nil || strings.Contains(err.Error(), "tok-secret") {
		t.Fatalf("request construction error = %v, want one without the token", err)
	}
	if !strings.Contains(redact(errors.New("lookup /bot"+testToken+"/x failed"), testToken).Error(), "[redacted]") {
		t.Fatal("redact did not scrub an embedded token")
	}
}
