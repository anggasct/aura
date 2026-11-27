package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/secret"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return fn(r)
}

func useInMemoryBotAPI(adapter *Adapter, api *fakeBotAPI) {
	adapter.bot.base = "https://telegram.test"
	adapter.bot.http.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		api.serve(rec, r)
		resp := rec.Result()
		resp.Request = r
		return resp, nil
	})
}

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
	adapter.cfg.MinEditInterval = configkit.Duration(minEdit)
	return adapter
}

func endPresence(t *testing.T, adapter *Adapter, turnID string) {
	t.Helper()
	if err := adapter.PresenceEnded(t.Context(), turnID, runtimechannelhost.PresenceEnd{}); err != nil {
		t.Fatalf("PresenceEnded: %v", err)
	}
}

func presenceUpdate(t *testing.T, adapter *Adapter, req *runtimechannelhost.DeliveryRequest, update runtimechannelhost.PresenceUpdate) {
	t.Helper()
	if err := adapter.PresenceUpdated(t.Context(), req, update); err != nil {
		t.Fatalf("PresenceUpdated(%+v): %v", update, err)
	}
}

func stepUpdate(callID, activity string, outcome runtimechannelhost.StepOutcome) runtimechannelhost.PresenceUpdate {
	return runtimechannelhost.PresenceUpdate{
		CallIDs:  []string{callID},
		Activity: activity,
		Outcome:  outcome,
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

func TestTypingContinuesAfterTheStatusIsPosted(t *testing.T) {
	t.Run("stops on Deliver", func(t *testing.T) {
		h := newHarness(t)
		adapter := h.adapter(t, nil)
		useInMemoryBotAPI(adapter, h.api)
		req := presenceRequest(t, h, "turn-typing-deliver")
		deliverReq := deliveryRequest(t, h, "turn-typing-deliver", "final answer")

		synctest.Test(t, func(t *testing.T) {
			if err := adapter.PresenceStarted(t.Context(), req); err != nil {
				t.Fatalf("PresenceStarted: %v", err)
			}
			synctest.Wait()
			if got := h.api.count("sendChatAction"); got != 1 {
				t.Fatalf("initial typing calls = %d, want 1", got)
			}

			time.Sleep(statusPostAfter)
			synctest.Wait()
			if got := h.api.count("sendMessage"); got != 1 {
				t.Fatalf("sendMessage calls at %v = %d, want 1", statusPostAfter, got)
			}
			posted := h.api.callsOf("sendMessage")[0]
			if posted.body["text"] != "Working…\n⏱ 0:10" {
				t.Errorf("initial status text = %q, want Working… and 0:10", posted.body["text"])
			}
			if _, formatted := posted.body["parse_mode"]; formatted {
				t.Error("status message was sent with a parse mode")
			}
			if _, replies := posted.body["reply_parameters"]; replies {
				t.Error("status message replies to the trigger")
			}

			time.Sleep(12 * time.Second)
			synctest.Wait()
			afterPost := 0
			for _, call := range h.api.callsOf("sendChatAction") {
				if call.at.After(posted.at) {
					afterPost++
				}
			}
			if afterPost < 3 {
				t.Fatalf("typing calls after status post = %d, want at least 3", afterPost)
			}

			if _, err := adapter.Deliver(t.Context(), deliverReq); err != nil {
				t.Fatalf("Deliver: %v", err)
			}
			typingAtDeliver := h.api.count("sendChatAction")
			time.Sleep(20 * time.Second)
			synctest.Wait()
			if got := h.api.count("sendChatAction"); got != typingAtDeliver {
				t.Fatalf("typing calls after Deliver = %d, want still %d", got, typingAtDeliver)
			}
			endPresence(t, adapter, "turn-typing-deliver")
		})
	})

	t.Run("stops on PresenceEnded", func(t *testing.T) {
		h := newHarness(t)
		adapter := h.adapter(t, nil)
		useInMemoryBotAPI(adapter, h.api)
		req := presenceRequest(t, h, "turn-typing-end")

		synctest.Test(t, func(t *testing.T) {
			if err := adapter.PresenceStarted(t.Context(), req); err != nil {
				t.Fatalf("PresenceStarted: %v", err)
			}
			time.Sleep(statusPostAfter + 12*time.Second)
			synctest.Wait()
			posted := h.api.callsOf("sendMessage")[0]
			afterPost := 0
			for _, call := range h.api.callsOf("sendChatAction") {
				if call.at.After(posted.at) {
					afterPost++
				}
			}
			if afterPost < 3 {
				t.Fatalf("typing calls after status post = %d, want at least 3", afterPost)
			}
			endPresence(t, adapter, "turn-typing-end")
			typingAtEnd := h.api.count("sendChatAction")
			time.Sleep(20 * time.Second)
			synctest.Wait()
			if got := h.api.count("sendChatAction"); got != typingAtEnd {
				t.Fatalf("typing calls after PresenceEnded = %d, want still %d", got, typingAtEnd)
			}
		})
	})
}

func TestElapsedTimeTicksWithoutActivity(t *testing.T) {
	t.Run("stops on Deliver", func(t *testing.T) {
		h := newHarness(t)
		adapter := h.adapter(t, nil)
		adapter.cfg.MinEditInterval = configkit.Duration(2 * time.Second)
		useInMemoryBotAPI(adapter, h.api)
		req := presenceRequest(t, h, "turn-tick-deliver")
		deliverReq := deliveryRequest(t, h, "turn-tick-deliver", "final answer")

		synctest.Test(t, func(t *testing.T) {
			if err := adapter.PresenceStarted(t.Context(), req); err != nil {
				t.Fatalf("PresenceStarted: %v", err)
			}
			time.Sleep(statusPostAfter)
			synctest.Wait()
			if got := h.api.count("sendMessage"); got != 1 {
				t.Fatalf("status posts = %d, want 1", got)
			}
			if got := h.api.sends()[0]["text"]; got != "Working…\n⏱ 0:10" {
				t.Fatalf("initial status = %q, want Working… at 0:10", got)
			}

			time.Sleep(90 * time.Second)
			synctest.Wait()
			edits := h.api.bodies("editMessageText")
			wantEdits := []string{
				"Working…\n⏱ 0:40",
				"Still working…\n⏱ 1:10",
				"Still working…\n⏱ 1:40",
			}
			if len(edits) != len(wantEdits) {
				t.Fatalf("idle edits = %d (%v), want %d", len(edits), edits, len(wantEdits))
			}
			for i, want := range wantEdits {
				if edits[i]["text"] != want {
					t.Errorf("edit[%d] = %q, want %q", i, edits[i]["text"], want)
				}
			}

			if _, err := adapter.Deliver(t.Context(), deliverReq); err != nil {
				t.Fatalf("Deliver: %v", err)
			}
			time.Sleep(90 * time.Second)
			synctest.Wait()
			if got := h.api.count("editMessageText"); got != len(wantEdits) {
				t.Fatalf("edits after Deliver = %d, want still %d", got, len(wantEdits))
			}
			endPresence(t, adapter, "turn-tick-deliver")
		})
	})

	t.Run("stops on PresenceEnded", func(t *testing.T) {
		h := newHarness(t)
		adapter := h.adapter(t, nil)
		adapter.cfg.MinEditInterval = configkit.Duration(2 * time.Second)
		useInMemoryBotAPI(adapter, h.api)
		req := presenceRequest(t, h, "turn-tick-end")

		synctest.Test(t, func(t *testing.T) {
			if err := adapter.PresenceStarted(t.Context(), req); err != nil {
				t.Fatalf("PresenceStarted: %v", err)
			}
			time.Sleep(statusPostAfter + 60*time.Second)
			synctest.Wait()
			if got := h.api.count("editMessageText"); got != 2 {
				t.Fatalf("idle edits = %d, want 2", got)
			}
			endPresence(t, adapter, "turn-tick-end")
			time.Sleep(90 * time.Second)
			synctest.Wait()
			if got := h.api.count("editMessageText"); got != 2 {
				t.Fatalf("edits after PresenceEnded = %d, want still 2", got)
			}
		})
	})
}

func TestPresenceFastTurnPostsNoStatusAndJournalsNothing(t *testing.T) {
	h := newHarness(t)
	adapter := presenceAdapter(t, h, time.Hour, time.Hour, 0)
	req := presenceRequest(t, h, "turn-fast")
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted: %v", err)
	}
	presenceUpdate(t, adapter, req, stepUpdate("c1", "Looking through notes", runtimechannelhost.StepRunning))
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
	presenceUpdate(t, adapter, req, stepUpdate("c1", "Looking through notes", runtimechannelhost.StepRunning))
	eventually(t, "status post", func() bool { return h.api.count("sendMessage") == 1 })
	presenceUpdate(t, adapter, req, stepUpdate("c1", "Looking through notes", runtimechannelhost.StepDone))
	presenceUpdate(t, adapter, req, stepUpdate("c2", "Checking open tasks", runtimechannelhost.StepRunning))
	endPresence(t, adapter, "turn-edit")
	posts := h.api.sends()
	firstPostText, _ := posts[0]["text"].(string)
	if len(posts) != 1 || !strings.HasPrefix(firstPostText, "⏳ Looking through notes\n⏱ ") {
		t.Fatalf("status posts = %v, want one post with the running step and elapsed time", posts)
	}
	edits := h.api.bodies("editMessageText")
	if len(edits) != 2 {
		t.Fatalf("edits = %d, want 2", len(edits))
	}
	lastText, _ := edits[1]["text"].(string)
	if !strings.HasPrefix(lastText, "✓ Looking through notes\n⏳ Checking open tasks\n⏱ ") || edits[1]["message_id"] != float64(701) || edits[1]["chat_id"] != float64(ownerUserID) {
		t.Errorf("edit = %v, want the status message edited in place with both steps", edits[1])
	}
	if _, formatted := edits[1]["parse_mode"]; formatted {
		t.Error("status edit was sent with a parse mode")
	}
	if states := operationStates(t, h, OpSendMessage); states[effect.StateSucceeded] != 1 {
		t.Errorf("send intents = %v, want one journaled status post", states)
	}
	if states := operationStates(t, h, opEditMessage); states[effect.StateSucceeded] != 2 {
		t.Errorf("edit intents = %v, want two journaled edits", states)
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
	presenceUpdate(t, adapter, req, stepUpdate("c1", "Second activity here", runtimechannelhost.StepRunning))
	presenceUpdate(t, adapter, req, stepUpdate("c2", "Third activity here", runtimechannelhost.StepRunning))
	if got := h.api.count("editMessageText"); got != 0 {
		t.Fatalf("edits = %d inside the interval, want 0", got)
	}
	eventually(t, "trailing edit", func() bool { return h.api.count("editMessageText") == 1 })
	endPresence(t, adapter, "turn-coalesce")
	edits := h.api.callsOf("editMessageText")
	text, _ := edits[0].body["text"].(string)
	if len(edits) != 1 || !strings.Contains(text, "⏳ Second activity here") || !strings.Contains(text, "⏳ Third activity here") {
		t.Fatalf("edits = %v, want one edit carrying both coalesced steps", edits)
	}
	posted := h.api.callsOf("sendMessage")[0]
	if gap := edits[0].at.Sub(posted.at); gap < interval {
		t.Errorf("edit followed the post after %v, want at least %v", gap, interval)
	}
}

func TestPresenceUnchangedStatusIsNotReEdited(t *testing.T) {
	h := newHarness(t)
	adapter := presenceAdapter(t, h, time.Nanosecond, time.Hour, 0)
	useInMemoryBotAPI(adapter, h.api)
	req := presenceRequest(t, h, "turn-unchanged")
	synctest.Test(t, func(t *testing.T) {
		if err := adapter.PresenceStarted(t.Context(), req); err != nil {
			t.Fatalf("PresenceStarted: %v", err)
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if got := h.api.count("sendMessage"); got != 1 {
			t.Fatalf("status posts = %d, want 1", got)
		}
		presenceUpdate(t, adapter, req, stepUpdate("c1", "Checking open tasks", runtimechannelhost.StepRunning))
		presenceUpdate(t, adapter, req, stepUpdate("c1", "Checking open tasks", runtimechannelhost.StepRunning))
		endPresence(t, adapter, "turn-unchanged")
		if got := h.api.count("editMessageText"); got != 1 {
			t.Fatalf("edits = %d, want 1 (an unchanged status is not re-sent)", got)
		}
		if states := operationStates(t, h, opEditMessage); states[effect.StateSucceeded] != 1 || len(states) != 1 {
			t.Fatalf("edit intents = %v, want the single edit recorded as success", states)
		}
	})
}

func TestPresenceStepFailureMarksOnlyThatStep(t *testing.T) {
	h := newHarness(t)
	adapter := presenceAdapter(t, h, time.Nanosecond, time.Hour, 0)
	useInMemoryBotAPI(adapter, h.api)
	req := presenceRequest(t, h, "turn-step-fail")
	synctest.Test(t, func(t *testing.T) {
		if err := adapter.PresenceStarted(t.Context(), req); err != nil {
			t.Fatalf("PresenceStarted: %v", err)
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		presenceUpdate(t, adapter, req, stepUpdate("c1", "Checking open tasks", runtimechannelhost.StepRunning))
		presenceUpdate(t, adapter, req, stepUpdate("c1", "Checking open tasks", runtimechannelhost.StepFailed))
		presenceUpdate(t, adapter, req, stepUpdate("c2", "Trying another source", runtimechannelhost.StepRunning))
		endPresence(t, adapter, "turn-step-fail")
		edits := h.api.bodies("editMessageText")
		if len(edits) != 3 {
			t.Fatalf("edits = %d, want 3", len(edits))
		}
		last, _ := edits[2]["text"].(string)
		if !strings.HasPrefix(last, "✗ Checking open tasks\n⏳ Trying another source\n⏱ ") {
			t.Errorf("last edit = %q, want failed first step and running second step", last)
		}
		if strings.Contains(last, "⚠️") {
			t.Errorf("last edit contains legacy global warning marker: %q", last)
		}
	})
}

func TestPresenceRedactsSecretsFromStatusMessages(t *testing.T) {
	h := newHarness(t)
	vault := secret.New()
	vault.Set("api", "sk-live-hunter2")
	adapter := h.adapterWithDeps(t, nil, nil, func(deps *Deps) {
		deps.SecretVault = vault
	})
	adapter.statusAfter = time.Nanosecond
	adapter.typingRefresh = time.Hour
	adapter.cfg.MinEditInterval = 0
	req := presenceRequest(t, h, "turn-secret")
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted: %v", err)
	}
	eventually(t, "status post", func() bool { return h.api.count("sendMessage") == 1 })
	presenceUpdate(t, adapter, req, stepUpdate("c1", "Using sk-live-hunter2 for search", runtimechannelhost.StepRunning))
	presenceUpdate(t, adapter, req, stepUpdate("c2", "Reading the password file", runtimechannelhost.StepRunning))
	presenceUpdate(t, adapter, req, stepUpdate("c1", "Using sk-live-hunter2 for search", runtimechannelhost.StepFailed))
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
	presenceUpdate(t, adapter, req, stepUpdate("c1", "Second activity here", runtimechannelhost.StepRunning))
	if elapsed := time.Since(started); elapsed >= adapter.rateCap {
		t.Fatalf("a rate-limited status edit blocked the turn for %v", elapsed)
	}
	presenceUpdate(t, adapter, req, stepUpdate("c2", "Third activity here", runtimechannelhost.StepRunning))
	if got := h.api.count("editMessageText"); got != 1 {
		t.Fatalf("edits = %d while rate limited, want only the rejected one", got)
	}
	eventually(t, "edit after retry_after", func() bool { return h.api.count("editMessageText") == 2 })
	endPresence(t, adapter, "turn-limited")
	edits := h.api.callsOf("editMessageText")
	text, _ := edits[1].body["text"].(string)
	if !strings.Contains(text, "⏳ Third activity here") {
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
	if err := adapter.PresenceEnded(t.Context(), "turn-ended", runtimechannelhost.PresenceEnd{}); err != nil {
		t.Fatalf("second PresenceEnded: %v", err)
	}
	if err := adapter.PresenceEnded(t.Context(), "", runtimechannelhost.PresenceEnd{}); err == nil {
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
			presenceUpdate(t, adapter, req, stepUpdate("c1", "Checking open tasks", runtimechannelhost.StepRunning))
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
	if err := adapter.PresenceUpdated(t.Context(), req, stepUpdate("c1", "x", runtimechannelhost.StepRunning)); err == nil {
		t.Fatal("presence without a turn id accepted")
	}
	if got := h.api.count("sendChatAction"); got != 0 {
		t.Fatalf("typing calls = %d, want 0", got)
	}
}

func TestPresenceUpdateDuringInFlightPostFlushesAfterMinEditInterval(t *testing.T) {
	h := newHarness(t)
	adapter := h.adapter(t, nil)
	adapter.cfg.MinEditInterval = configkit.Duration(2 * time.Second)
	req := presenceRequest(t, h, "turn-inflight-flush")

	synctest.Test(t, func(t *testing.T) {
		postStarted := make(chan struct{})
		releasePost := make(chan struct{})
		var sendCalls atomic.Int32
		adapter.bot.base = "https://telegram.test"
		adapter.bot.http.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "/sendMessage") && sendCalls.Add(1) == 1 {
				close(postStarted)
				<-releasePost
			}
			rec := httptest.NewRecorder()
			h.api.serve(rec, r)
			resp := rec.Result()
			resp.Request = r
			return resp, nil
		})

		if err := adapter.PresenceStarted(t.Context(), req); err != nil {
			t.Fatalf("PresenceStarted: %v", err)
		}
		adapter.presenceMu.Lock()
		session := adapter.presence["turn-inflight-flush"]
		adapter.presenceMu.Unlock()

		time.Sleep(statusPostAfter)
		<-postStarted

		updateDone := make(chan error, 1)
		go func() {
			updateDone <- adapter.PresenceUpdated(t.Context(), req, stepUpdate("c1", "Inspecting logs", runtimechannelhost.StepRunning))
		}()
		for {
			adapter.presenceMu.Lock()
			updated := session.version > 0
			adapter.presenceMu.Unlock()
			if updated {
				break
			}
			runtime.Gosched()
		}

		close(releasePost)
		if err := <-updateDone; err != nil {
			t.Fatalf("PresenceUpdated: %v", err)
		}
		synctest.Wait()
		if got := h.api.count("editMessageText"); got != 0 {
			t.Fatalf("edits immediately after post = %d, want 0 within min_edit_interval", got)
		}

		time.Sleep(2 * time.Second)
		synctest.Wait()
		edits := h.api.bodies("editMessageText")
		if len(edits) != 1 {
			t.Fatalf("edits after min_edit_interval = %d, want 1", len(edits))
		}
		text, _ := edits[0]["text"].(string)
		if !strings.HasPrefix(text, "⏳ Inspecting logs\n⏱ ") {
			t.Errorf("flushed edit = %q, want step queued during in-flight post", text)
		}
		endPresence(t, adapter, "turn-inflight-flush")
	})
}

func TestInitialStatusDefersDuringTypingRateHold(t *testing.T) {
	h := newHarness(t)
	adapter := h.adapter(t, nil)
	adapter.rateCap = rateLimitCap
	adapter.cfg.MinEditInterval = configkit.Duration(2 * time.Second)
	useInMemoryBotAPI(adapter, h.api)
	req := presenceRequest(t, h, "turn-typing-hold")

	synctest.Test(t, func(t *testing.T) {
		if err := adapter.PresenceStarted(t.Context(), req); err != nil {
			t.Fatalf("PresenceStarted: %v", err)
		}
		time.Sleep(typingRefreshInterval)
		synctest.Wait()
		h.api.failNextOn("sendChatAction", apiFailure(http.StatusTooManyRequests, 5))
		time.Sleep(typingRefreshInterval)
		synctest.Wait()

		time.Sleep(statusPostAfter - 2*typingRefreshInterval)
		synctest.Wait()
		if got := h.api.count("sendMessage"); got != 0 {
			t.Fatalf("sendMessage during active typing rate hold = %d, want 0", got)
		}

		time.Sleep(2 * time.Second)
		synctest.Wait()
		if got := h.api.count("sendMessage"); got != 0 {
			t.Fatalf("sendMessage before rate hold expiry = %d, want 0", got)
		}

		time.Sleep(time.Second)
		synctest.Wait()
		sends := h.api.sends()
		if len(sends) != 1 {
			t.Fatalf("sendMessage after rate hold expiry = %d, want 1", len(sends))
		}
		text, _ := sends[0]["text"].(string)
		if text != "Working…\n⏱ 0:13" {
			t.Errorf("deferred initial status = %q, want Working… at 0:13", text)
		}
		endPresence(t, adapter, "turn-typing-hold")
	})
}

func TestStatusTextsComeFromThePersona(t *testing.T) {
	registry, err := auraagent.Build([]auraagent.Persona{
		{
			ID:           "kantor",
			Description:  "Office assistant",
			Instructions: "Help.",
			Messages: map[string]string{
				auraagent.MessageStatusWorking:      "Lagi dicek…",
				auraagent.MessageStatusStillWorking: "Masih dicek…",
				auraagent.MessageStatusStopped:      "⏹ Batal",
				auraagent.MessageButtonStop:         "Batal",
			},
		},
	}, []string{"read_file", "write_file", "list_dir", "exec", "web_fetch", "web_search"}, nil, []string{"primary"}, "id")
	if err != nil {
		t.Fatalf("agent.Build: %v", err)
	}
	router, err := auraagent.NewRouter(registry, []auraagent.ChannelAccount{{
		Channel: auraagent.BindingChannelTelegram,
		Name:    "pocket",
		Persona: "main",
		Field:   "channels.telegram.accounts[0]",
	}}, nil)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	cases := []struct {
		name        string
		agentID     string
		wantWorking string
		wantStill   string
		wantButton  string
		wantStopped string
	}{
		{
			name:        "id bundle default",
			agentID:     "main",
			wantWorking: "Sedang mengerjakan…\n⏱ 0:10",
			wantStill:   "Masih mengerjakan…\n⏱ 1:10",
			wantButton:  "Hentikan",
			wantStopped: "⏹ Dihentikan",
		},
		{
			name:        "persona override wins",
			agentID:     "kantor",
			wantWorking: "Lagi dicek…\n⏱ 0:10",
			wantStill:   "Masih dicek…\n⏱ 1:10",
			wantButton:  "Batal",
			wantStopped: "⏹ Batal",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			adapter := h.adapterWithDeps(t, nil, nil, func(d *Deps) {
				d.PersonaRouter = router
			})
			useInMemoryBotAPI(adapter, h.api)
			req := stopPresenceRequest(t, h, "turn-loc-"+tc.agentID, principal.ClassOwner, replyReference{
				ChatID:    ownerUserID,
				MessageID: 100,
				FromID:    ownerUserID,
			})
			req.AgentID = tc.agentID

			synctest.Test(t, func(t *testing.T) {
				req.Deadline = time.Now().Add(5 * time.Minute)
				if err := adapter.PresenceStarted(t.Context(), req); err != nil {
					t.Fatalf("PresenceStarted: %v", err)
				}
				time.Sleep(statusPostAfter)
				synctest.Wait()

				sends := h.api.sends()
				if len(sends) != 1 {
					t.Fatalf("sendMessage count = %d, want 1", len(sends))
				}
				if sends[0]["text"] != tc.wantWorking {
					t.Fatalf("initial status = %q, want %q", sends[0]["text"], tc.wantWorking)
				}
				if label, _, ok := extractTelegramStopButton(sends[0]); !ok || label != tc.wantButton {
					t.Fatalf("stop button = (%q, %v), want %q", label, ok, tc.wantButton)
				}

				time.Sleep(time.Minute)
				synctest.Wait()
				edits := h.api.bodies("editMessageText")
				if len(edits) < 2 || edits[len(edits)-1]["text"] != tc.wantStill {
					t.Fatalf("still-working edits = %v, want last %q", edits, tc.wantStill)
				}
				if label, _, ok := extractTelegramStopButton(edits[len(edits)-1]); !ok || label != tc.wantButton {
					t.Fatalf("edited stop button = (%q, %v), want %q", label, ok, tc.wantButton)
				}

				if err := adapter.PresenceEnded(t.Context(), req.EffectID, runtimechannelhost.PresenceEnd{Stopped: true}); err != nil {
					t.Fatalf("PresenceEnded: %v", err)
				}
				edits = h.api.bodies("editMessageText")
				if edits[len(edits)-1]["text"] != tc.wantStopped {
					t.Fatalf("stopped status = %q, want %q", edits[len(edits)-1]["text"], tc.wantStopped)
				}
			})
		})
	}
}
