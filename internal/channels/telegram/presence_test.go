package telegram

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/channels/kit/status"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/secret"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
)

func presenceRequest(t *testing.T, h *harness, turnID string) *runtimechannelhost.DeliveryRequest {
	t.Helper()
	req := deliveryRequest(t, h, turnID, "")
	req.Parts = nil
	return req
}

func presenceAdapter(t *testing.T, h *harness, statusAfter, typingRefresh, minEdit time.Duration) *Adapter {
	t.Helper()
	adapter := h.adapter(t, nil)
	adapter.statusAfter = statusAfter
	adapter.typingRefresh = typingRefresh
	adapter.cfg.MinEditInterval = config.Duration(minEdit)
	return adapter
}

func endPresence(t *testing.T, adapter *Adapter, turnID string) {
	t.Helper()
	if err := adapter.PresenceEnded(t.Context(), turnID); err != nil {
		t.Fatalf("PresenceEnded: %v", err)
	}
}

func presenceUpdate(t *testing.T, adapter *Adapter, req *runtimechannelhost.DeliveryRequest, text, args string, failed bool) {
	t.Helper()
	if err := adapter.PresenceUpdated(t.Context(), req, text, args, failed); err != nil {
		t.Fatalf("PresenceUpdated(%q): %v", text, err)
	}
}

func operationStates(t *testing.T, h *harness, operation string) map[effect.State]int {
	t.Helper()
	rows, err := h.db.QueryContext(t.Context(), `SELECT state FROM effect_intent WHERE provider = 'telegram' AND operation = ?`, operation)
	if err != nil {
		t.Fatalf("query intents: %v", err)
	}
	defer func() { _ = rows.Close() }()
	states := map[effect.State]int{}
	for rows.Next() {
		var state string
		if err := rows.Scan(&state); err != nil {
			t.Fatalf("scan: %v", err)
		}
		states[effect.State(state)]++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate intents: %v", err)
	}
	return states
}

func TestPresenceTypingRefreshesUntilTheStatusIsPosted(t *testing.T) {
	h := newHarness(t)
	adapter := presenceAdapter(t, h, 120*time.Millisecond, 15*time.Millisecond, 0)
	req := presenceRequest(t, h, "turn-typing")
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted: %v", err)
	}
	typing := h.api.bodies("sendChatAction")
	if len(typing) != 1 || typing[0]["action"] != chatActionTyping || typing[0]["chat_id"] != float64(ownerUserID) {
		t.Fatalf("typing calls = %v, want one immediate typing action for the chat", typing)
	}
	eventually(t, "typing refresh", func() bool { return h.api.count("sendChatAction") >= 3 })
	eventually(t, "status post", func() bool { return h.api.count("sendMessage") == 1 })
	endPresence(t, adapter, "turn-typing")
	posted := h.api.callsOf("sendMessage")[0]
	for _, call := range h.api.callsOf("sendChatAction") {
		if call.at.After(posted.at) {
			t.Fatal("typing continued after the status message was posted")
		}
	}
	if posted.body["text"] != status.Fallback(0) {
		t.Errorf("status text = %v, want the first fallback", posted.body["text"])
	}
	if _, formatted := posted.body["parse_mode"]; formatted {
		t.Error("status message was sent with a parse mode")
	}
	if _, replies := posted.body["reply_parameters"]; replies {
		t.Error("status message replies to the trigger")
	}
}

func TestPresenceFastTurnPostsNoStatusAndJournalsNothing(t *testing.T) {
	h := newHarness(t)
	adapter := presenceAdapter(t, h, time.Hour, time.Hour, 0)
	req := presenceRequest(t, h, "turn-fast")
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted: %v", err)
	}
	presenceUpdate(t, adapter, req, "Looking through notes", "", false)
	endPresence(t, adapter, "turn-fast")
	if got := h.api.count("sendMessage"); got != 0 {
		t.Fatalf("sendMessage calls = %d, want none for a fast turn", got)
	}
	if got := h.api.count("sendChatAction"); got != 1 {
		t.Fatalf("typing calls = %d, want 1", got)
	}
	if states := intentStates(t, h); len(states) != 0 {
		t.Fatalf("intent states = %v, want typing to stay unjournaled", states)
	}
}

func TestPresencePostsOnceThenEditsInPlace(t *testing.T) {
	h := newHarness(t)
	adapter := presenceAdapter(t, h, 30*time.Millisecond, time.Hour, 0)
	req := presenceRequest(t, h, "turn-edit")
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted: %v", err)
	}
	presenceUpdate(t, adapter, req, "Looking through notes", `{"path":"notes.md"}`, false)
	eventually(t, "status post", func() bool { return h.api.count("sendMessage") == 1 })
	presenceUpdate(t, adapter, req, "Checking open tasks", "", false)
	endPresence(t, adapter, "turn-edit")
	posts := h.api.sends()
	if len(posts) != 1 || posts[0]["text"] != "Looking through notes notes.md" {
		t.Fatalf("status posts = %v, want one post with the rendered activity", posts)
	}
	edits := h.api.bodies("editMessageText")
	if len(edits) != 1 {
		t.Fatalf("edits = %d, want 1", len(edits))
	}
	if edits[0]["text"] != "Checking open tasks" || edits[0]["message_id"] != float64(701) || edits[0]["chat_id"] != float64(ownerUserID) {
		t.Errorf("edit = %v, want the status message edited in place", edits[0])
	}
	if _, formatted := edits[0]["parse_mode"]; formatted {
		t.Error("status edit was sent with a parse mode")
	}
	if states := operationStates(t, h, OpSendMessage); states[effect.StateSucceeded] != 1 {
		t.Errorf("send intents = %v, want one journaled status post", states)
	}
	if states := operationStates(t, h, opEditMessage); states[effect.StateSucceeded] != 1 {
		t.Errorf("edit intents = %v, want one journaled edit", states)
	}
}

func TestPresenceCoalescesEditsUnderMinEditInterval(t *testing.T) {
	h := newHarness(t)
	interval := 150 * time.Millisecond
	adapter := presenceAdapter(t, h, time.Nanosecond, time.Hour, interval)
	req := presenceRequest(t, h, "turn-coalesce")
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted: %v", err)
	}
	eventually(t, "status post", func() bool { return h.api.count("sendMessage") == 1 })
	presenceUpdate(t, adapter, req, "Second activity here", "", false)
	presenceUpdate(t, adapter, req, "Third activity here", "", false)
	if got := h.api.count("editMessageText"); got != 0 {
		t.Fatalf("edits = %d inside the interval, want 0", got)
	}
	eventually(t, "trailing edit", func() bool { return h.api.count("editMessageText") == 1 })
	endPresence(t, adapter, "turn-coalesce")
	edits := h.api.callsOf("editMessageText")
	if len(edits) != 1 || edits[0].body["text"] != "Third activity here" {
		t.Fatalf("edits = %v, want one edit carrying the latest activity", edits)
	}
	posted := h.api.callsOf("sendMessage")[0]
	if gap := edits[0].at.Sub(posted.at); gap < interval {
		t.Errorf("edit followed the post after %v, want at least %v", gap, interval)
	}
}

func TestPresenceUnchangedStatusIsNotReEdited(t *testing.T) {
	h := newHarness(t)
	adapter := presenceAdapter(t, h, time.Nanosecond, time.Hour, 0)
	req := presenceRequest(t, h, "turn-unchanged")
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted: %v", err)
	}
	eventually(t, "status post", func() bool { return h.api.count("sendMessage") == 1 })
	presenceUpdate(t, adapter, req, "Checking open tasks", "", false)
	presenceUpdate(t, adapter, req, "Checking open tasks", "", false)
	endPresence(t, adapter, "turn-unchanged")
	if got := h.api.count("editMessageText"); got != 1 {
		t.Fatalf("edits = %d, want 1 (an unchanged status is not re-sent)", got)
	}
	if states := operationStates(t, h, opEditMessage); states[effect.StateSucceeded] != 1 || len(states) != 1 {
		t.Fatalf("edit intents = %v, want the single edit recorded as success", states)
	}
}

func TestPresenceFailureLineIsShownOnce(t *testing.T) {
	h := newHarness(t)
	adapter := presenceAdapter(t, h, time.Nanosecond, time.Hour, 0)
	req := presenceRequest(t, h, "turn-failure")
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted: %v", err)
	}
	eventually(t, "status post", func() bool { return h.api.count("sendMessage") == 1 })
	presenceUpdate(t, adapter, req, "Checking open tasks", "", false)
	presenceUpdate(t, adapter, req, "", "", true)
	presenceUpdate(t, adapter, req, "", "", true)
	endPresence(t, adapter, "turn-failure")
	edits := h.api.bodies("editMessageText")
	if len(edits) != 2 {
		t.Fatalf("edits = %d, want the activity and one failure line", len(edits))
	}
	if want := status.FailureLine("Checking open tasks"); edits[1]["text"] != want {
		t.Errorf("failure edit = %v, want %q", edits[1]["text"], want)
	}
}

func TestPresenceRedactsSecretsFromStatusMessages(t *testing.T) {
	h := newHarness(t)
	adapter := presenceAdapter(t, h, time.Nanosecond, time.Hour, 0)
	vault := secret.New()
	vault.Set("api", "sk-live-hunter2")
	adapter.SetSecretVault(vault)
	req := presenceRequest(t, h, "turn-secret")
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted: %v", err)
	}
	eventually(t, "status post", func() bool { return h.api.count("sendMessage") == 1 })
	presenceUpdate(t, adapter, req, "Using sk-live-hunter2 for search", `{"q":"sk-live-hunter2"}`, false)
	presenceUpdate(t, adapter, req, "Reading the password file", "", false)
	presenceUpdate(t, adapter, req, "Calling sk-live-hunter2", "", true)
	endPresence(t, adapter, "turn-secret")
	for _, method := range []string{"sendMessage", "editMessageText"} {
		for _, body := range h.api.bodies(method) {
			text, _ := body["text"].(string)
			if strings.Contains(text, "hunter2") || strings.Contains(strings.ToLower(text), "password") {
				t.Fatalf("%s leaked a secret: %q", method, text)
			}
		}
	}
	if h.api.count("editMessageText") == 0 {
		t.Fatal("no status edit was attempted")
	}
}

func TestPresenceRateLimitDefersTheEditUntilRetryAfter(t *testing.T) {
	h := newHarness(t)
	adapter := presenceAdapter(t, h, time.Nanosecond, time.Hour, 0)
	adapter.rateCap = time.Second
	req := presenceRequest(t, h, "turn-limited")
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted: %v", err)
	}
	eventually(t, "status post", func() bool { return h.api.count("sendMessage") == 1 })
	h.api.failNextOn("editMessageText", apiFailure(http.StatusTooManyRequests, 1))
	started := time.Now()
	presenceUpdate(t, adapter, req, "Second activity here", "", false)
	if elapsed := time.Since(started); elapsed >= adapter.rateCap {
		t.Fatalf("a rate-limited status edit blocked the turn for %v", elapsed)
	}
	presenceUpdate(t, adapter, req, "Third activity here", "", false)
	if got := h.api.count("editMessageText"); got != 1 {
		t.Fatalf("edits = %d while rate limited, want only the rejected one", got)
	}
	eventually(t, "edit after retry_after", func() bool { return h.api.count("editMessageText") == 2 })
	endPresence(t, adapter, "turn-limited")
	edits := h.api.callsOf("editMessageText")
	if edits[1].body["text"] != "Third activity here" {
		t.Errorf("deferred edit = %v, want the latest activity", edits[1].body["text"])
	}
	if gap := edits[1].at.Sub(edits[0].at); gap < adapter.rateCap {
		t.Errorf("deferred edit came after %v, want at least the capped retry_after %v", gap, adapter.rateCap)
	}
	if states := operationStates(t, h, opEditMessage); states[effect.StateFailed] != 1 || states[effect.StateSucceeded] != 1 {
		t.Errorf("edit intents = %v, want one rate-limited failure and one success", states)
	}
}

func TestPresenceEndedStopsTheClockDeterministically(t *testing.T) {
	h := newHarness(t)
	adapter := presenceAdapter(t, h, 40*time.Millisecond, 10*time.Millisecond, 0)
	req := presenceRequest(t, h, "turn-ended")
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted: %v", err)
	}
	adapter.presenceMu.Lock()
	session := adapter.presence["turn-ended"]
	adapter.presenceMu.Unlock()
	endPresence(t, adapter, "turn-ended")
	select {
	case <-session.done:
	default:
		t.Fatal("PresenceEnded returned before the presence clock stopped")
	}
	adapter.presenceMu.Lock()
	remaining := len(adapter.presence)
	adapter.presenceMu.Unlock()
	if remaining != 0 {
		t.Fatalf("presence sessions = %d after end, want 0", remaining)
	}
	if err := adapter.PresenceEnded(t.Context(), "turn-ended"); err != nil {
		t.Fatalf("second PresenceEnded: %v", err)
	}
	if err := adapter.PresenceEnded(t.Context(), ""); err == nil {
		t.Fatal("PresenceEnded accepted an empty turn id")
	}
}

func TestPresenceClockStopsWhenTheTurnContextEnds(t *testing.T) {
	h := newHarness(t)
	adapter := presenceAdapter(t, h, time.Hour, time.Hour, 0)
	req := presenceRequest(t, h, "turn-cancelled")
	ctx, cancel := context.WithCancel(t.Context())
	if err := adapter.PresenceStarted(ctx, req); err != nil {
		t.Fatalf("PresenceStarted: %v", err)
	}
	adapter.presenceMu.Lock()
	session := adapter.presence["turn-cancelled"]
	adapter.presenceMu.Unlock()
	cancel()
	select {
	case <-session.done:
	case <-time.After(5 * time.Second):
		t.Fatal("presence clock outlived the turn context")
	}
	endPresence(t, adapter, "turn-cancelled")
}

func TestDeliverStopsPresenceAndSendsTheFinalReplySeparately(t *testing.T) {
	h := newHarness(t)
	adapter := presenceAdapter(t, h, time.Nanosecond, time.Hour, 0)
	req := presenceRequest(t, h, "turn-final")
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted: %v", err)
	}
	eventually(t, "status post", func() bool { return h.api.count("sendMessage") == 1 })
	adapter.presenceMu.Lock()
	session := adapter.presence["turn-final"]
	adapter.presenceMu.Unlock()
	receipt, err := adapter.Deliver(t.Context(), deliveryRequest(t, h, "turn-final", "**final** answer"))
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	select {
	case <-session.done:
	default:
		t.Fatal("Deliver did not stop the presence clock first")
	}
	sends := h.api.sends()
	if len(sends) != 2 {
		t.Fatalf("sendMessage calls = %d, want the status and the final reply", len(sends))
	}
	if sends[1]["text"] != "<b>final</b> answer" || sends[1]["parse_mode"] != parseModeHTML {
		t.Errorf("final reply = %v, want formatted HTML", sends[1])
	}
	if receipt.ExternalID != "702" {
		t.Errorf("receipt = %+v, want the final reply's message id", receipt)
	}
	endPresence(t, adapter, "turn-final")
}

func TestPresenceStatusFailuresNeverAffectTheFinalReply(t *testing.T) {
	cases := map[string]func(h *harness){
		"status post unknown": func(h *harness) {
			h.api.failNextOn("sendMessage", apiFailure(http.StatusBadGateway, 0))
		},
		"status edit rejected": func(h *harness) {
			h.api.failNextOn("editMessageText", describedFailure(http.StatusBadRequest, "Bad Request: message to edit not found", 0))
		},
		"typing rejected": func(h *harness) {
			h.api.failNextOn("sendChatAction", apiFailure(http.StatusInternalServerError, 0))
		},
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			adapter := presenceAdapter(t, h, time.Nanosecond, time.Hour, 0)
			script(h)
			req := presenceRequest(t, h, "turn-resilient")
			if err := adapter.PresenceStarted(t.Context(), req); err != nil {
				t.Fatalf("PresenceStarted: %v", err)
			}
			eventually(t, "status post attempt", func() bool { return h.api.count("sendMessage") == 1 })
			presenceUpdate(t, adapter, req, "Checking open tasks", "", false)
			if _, err := adapter.Deliver(t.Context(), deliveryRequest(t, h, "turn-resilient", "final answer")); err != nil {
				t.Fatalf("Deliver after a presence failure: %v", err)
			}
			endPresence(t, adapter, "turn-resilient")
			sends := h.api.sends()
			if last := sends[len(sends)-1]; last["text"] != "final answer" {
				t.Fatalf("last send = %v, want the final reply", last)
			}
			logs := h.logs.String()
			for _, leaked := range []string{testToken, "Checking open tasks", "final answer"} {
				if strings.Contains(logs, leaked) {
					t.Fatalf("logs leaked %q: %s", leaked, logs)
				}
			}
		})
	}
}

func TestPresenceRejectsRequestsWithoutAChat(t *testing.T) {
	h := newHarness(t)
	adapter := presenceAdapter(t, h, time.Hour, time.Hour, 0)
	req := presenceRequest(t, h, "turn-nochat")
	req.ReplyContext = []byte(`{"message_id":1}`)
	if err := adapter.PresenceStarted(t.Context(), req); err == nil {
		t.Fatal("presence without a chat accepted")
	}
	if err := adapter.PresenceStarted(t.Context(), nil); err == nil {
		t.Fatal("nil presence request accepted")
	}
	req = presenceRequest(t, h, "")
	if err := adapter.PresenceUpdated(t.Context(), req, "x", "", false); err == nil {
		t.Fatal("presence without a turn id accepted")
	}
	if got := h.api.count("sendChatAction"); got != 0 {
		t.Fatalf("typing calls = %d, want 0", got)
	}
}
