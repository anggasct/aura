package telegram

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/anggasct/aura/internal/channels/kit/status"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/fault"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
)

func (f *fakeBotAPI) callSequence() []botCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]botCall(nil), f.calls...)
}

func replyContextJSON(t *testing.T, ref replyReference) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(ref)
	if err != nil {
		t.Fatalf("marshal reply context: %v", err)
	}
	return raw
}

func statusTurnRequest(t *testing.T, h *harness, turnID string, ref replyReference, text string) *runtimechannelhost.DeliveryRequest {
	t.Helper()
	if err := h.sessions.EnsureSession(t.Context(), deliverySession, "local-owner", testPersona); err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	req := &runtimechannelhost.DeliveryRequest{
		EffectID:       turnID,
		Channel:        "telegram:pocket",
		ConversationID: deliverySession,
		ReplyContext:   replyContextJSON(t, ref),
		IdempotencyKey: turnID,
	}
	if text != "" {
		req.Parts = []runtimechannelhost.OutputPart{{Text: text}}
	}
	return req
}

func TestAnswerIsPostedNewAndTheStatusIsDeleted(t *testing.T) {
	h := newHarness(t)
	adapter := h.adapter(t, nil)
	useInMemoryBotAPI(adapter, h.api)
	ref := replyReference{ChatID: ownerUserID, MessageID: 100, ChatType: chatTypePrivate, FromID: ownerUserID}
	presenceReq := statusTurnRequest(t, h, "turn-new-delete", ref, "")
	deliverReq := statusTurnRequest(t, h, "turn-new-delete", ref, "final answer")

	synctest.Test(t, func(t *testing.T) {
		if err := adapter.PresenceStarted(t.Context(), presenceReq); err != nil {
			t.Fatalf("PresenceStarted: %v", err)
		}
		time.Sleep(statusPostAfter)
		synctest.Wait()

		if got := h.api.count("sendMessage"); got != 1 {
			t.Fatalf("status posts = %d, want 1", got)
		}

		receipt, err := adapter.Deliver(t.Context(), deliverReq)
		if err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		if receipt.ExternalID != "702" {
			t.Errorf("receipt = %+v, want message id 702", receipt)
		}

		sends := h.api.sends()
		if len(sends) != 2 {
			t.Fatalf("sendMessage calls = %d, want 2 (status and answer)", len(sends))
		}
		if sends[1]["text"] != "final answer" {
			t.Errorf("final send text = %q, want final answer", sends[1]["text"])
		}
		if got := h.api.count("editMessageText"); got != 0 {
			t.Errorf("editMessageText calls = %d, want 0", got)
		}

		deletes := h.api.bodies("deleteMessage")
		if len(deletes) != 1 {
			t.Fatalf("deleteMessage calls = %d, want 1", len(deletes))
		}
		if deletes[0]["chat_id"] != float64(ownerUserID) || deletes[0]["message_id"] != float64(701) {
			t.Errorf("deleteMessage body = %v, want chat %d message 701", deletes[0], ownerUserID)
		}

		seq := h.api.callSequence()
		lastSendIdx, deleteIdx := -1, -1
		for i, call := range seq {
			if call.method == "sendMessage" {
				lastSendIdx = i
			}
			if call.method == "deleteMessage" {
				deleteIdx = i
			}
		}
		if lastSendIdx < 0 || deleteIdx < 0 || deleteIdx < lastSendIdx {
			t.Errorf("call order: last sendMessage at %d, deleteMessage at %d, want delete after final send", lastSendIdx, deleteIdx)
		}

		var key, classification, state string
		if err := h.db.QueryRowContext(
			t.Context(),
			`SELECT idempotency_key, classification, state FROM effect_intent WHERE provider = 'telegram' AND operation = ?`,
			opDeleteMessage,
		).Scan(&key, &classification, &state); err != nil {
			t.Fatalf("query delete intent: %v", err)
		}
		wantKey := deleteKey("turn-new-delete", 701)
		if key != wantKey {
			t.Errorf("delete idempotency key = %q, want %q", key, wantKey)
		}
		if classification != string(effect.ClassificationEffectful) || state != string(effect.StateSucceeded) {
			t.Errorf("delete intent = (%s, %s), want (%s, %s)", classification, state, effect.ClassificationEffectful, effect.StateSucceeded)
		}

		endPresence(t, adapter, "turn-new-delete")
	})
}

func TestStatusDeleteToleratesAlreadyGone(t *testing.T) {
	h := newHarness(t)
	adapter := h.adapter(t, nil)
	useInMemoryBotAPI(adapter, h.api)
	ref := replyReference{ChatID: ownerUserID, MessageID: 100, ChatType: chatTypePrivate, FromID: ownerUserID}
	presenceReq := statusTurnRequest(t, h, "turn-already-gone", ref, "")
	deliverReq := statusTurnRequest(t, h, "turn-already-gone", ref, "final answer")

	synctest.Test(t, func(t *testing.T) {
		if err := adapter.PresenceStarted(t.Context(), presenceReq); err != nil {
			t.Fatalf("PresenceStarted: %v", err)
		}
		time.Sleep(statusPostAfter)
		synctest.Wait()

		h.api.failNextOn("deleteMessage", describedFailure(http.StatusBadRequest, "Bad Request: message to delete not found", 0))
		receipt, err := adapter.Deliver(t.Context(), deliverReq)
		if err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		if receipt.ExternalID != "702" {
			t.Errorf("receipt = %+v, want message id 702", receipt)
		}
		if got := h.api.count("deleteMessage"); got != 1 {
			t.Fatalf("deleteMessage calls = %d, want 1", got)
		}
		if states := operationStates(t, h, opDeleteMessage); states[effect.StateSucceeded] != 1 || len(states) != 1 {
			t.Errorf("delete intent states = %v, want 1 succeeded", states)
		}
		if strings.Contains(h.logs.String(), "status delete failed") {
			t.Errorf("unexpected status delete failed log: %s", h.logs.String())
		}
		endPresence(t, adapter, "turn-already-gone")
	})
}

func TestStatusDeleteFailureKeepsTheDelivery(t *testing.T) {
	t.Run("rejection", func(t *testing.T) {
		h := newHarness(t)
		adapter := h.adapter(t, nil)
		useInMemoryBotAPI(adapter, h.api)
		ref := replyReference{ChatID: ownerUserID, MessageID: 100, ChatType: chatTypePrivate, FromID: ownerUserID}
		presenceReq := statusTurnRequest(t, h, "turn-delete-reject", ref, "")
		deliverReq := statusTurnRequest(t, h, "turn-delete-reject", ref, "final answer")

		synctest.Test(t, func(t *testing.T) {
			if err := adapter.PresenceStarted(t.Context(), presenceReq); err != nil {
				t.Fatalf("PresenceStarted: %v", err)
			}
			time.Sleep(statusPostAfter)
			synctest.Wait()

			h.api.failNextOn("deleteMessage", describedFailure(http.StatusBadRequest, "Bad Request: message can't be deleted for everyone", 0))
			receipt, err := adapter.Deliver(t.Context(), deliverReq)
			if err != nil {
				t.Fatalf("Deliver failed on delete rejection: %v", err)
			}
			if receipt.ExternalID != "702" {
				t.Errorf("receipt = %+v, want message id 702", receipt)
			}
			if states := operationStates(t, h, opDeleteMessage); states[effect.StateFailed] != 1 {
				t.Errorf("delete intent states = %v, want 1 failed", states)
			}
			if !strings.Contains(h.logs.String(), "status delete failed") {
				t.Errorf("logs = %s, want status delete failed", h.logs.String())
			}
			endPresence(t, adapter, "turn-delete-reject")
		})
	})

	t.Run("timeout", func(t *testing.T) {
		h := newHarness(t)
		adapter := h.adapter(t, nil)
		adapter.deleteTimeout = 20 * time.Millisecond
		adapter.bot.base = "https://telegram.test"
		adapter.bot.http.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "/deleteMessage") {
				<-r.Context().Done()
				return nil, r.Context().Err()
			}
			rec := httptest.NewRecorder()
			h.api.serve(rec, r)
			resp := rec.Result()
			resp.Request = r
			return resp, nil
		})
		ref := replyReference{ChatID: ownerUserID, MessageID: 100, ChatType: chatTypePrivate, FromID: ownerUserID}
		presenceReq := statusTurnRequest(t, h, "turn-delete-timeout", ref, "")
		deliverReq := statusTurnRequest(t, h, "turn-delete-timeout", ref, "final answer")

		synctest.Test(t, func(t *testing.T) {
			if err := adapter.PresenceStarted(t.Context(), presenceReq); err != nil {
				t.Fatalf("PresenceStarted: %v", err)
			}
			time.Sleep(statusPostAfter)
			synctest.Wait()

			receipt, err := adapter.Deliver(t.Context(), deliverReq)
			if err != nil {
				t.Fatalf("Deliver failed on delete timeout: %v", err)
			}
			if receipt.ExternalID != "702" {
				t.Errorf("receipt = %+v, want message id 702", receipt)
			}
			if states := operationStates(t, h, opDeleteMessage); states[effect.StateUnknown] != 1 {
				t.Errorf("delete intent states = %v, want 1 unknown", states)
			}
			if !strings.Contains(h.logs.String(), "status delete failed") {
				t.Errorf("logs = %s, want status delete failed", h.logs.String())
			}
			endPresence(t, adapter, "turn-delete-timeout")
		})
	})
}

func TestFailedFinalMessageKeepsTheStatus(t *testing.T) {
	h := newHarness(t)
	adapter := h.adapter(t, nil)
	useInMemoryBotAPI(adapter, h.api)
	ref := replyReference{ChatID: ownerUserID, MessageID: 100, ChatType: chatTypePrivate, FromID: ownerUserID}
	presenceReq := statusTurnRequest(t, h, "turn-failed-final", ref, "")
	deliverReq := statusTurnRequest(t, h, "turn-failed-final", ref, "final answer")

	synctest.Test(t, func(t *testing.T) {
		if err := adapter.PresenceStarted(t.Context(), presenceReq); err != nil {
			t.Fatalf("PresenceStarted: %v", err)
		}
		time.Sleep(statusPostAfter)
		synctest.Wait()

		h.api.failNextOn("sendMessage", apiFailure(http.StatusForbidden, 0))
		_, err := adapter.Deliver(t.Context(), deliverReq)
		if !fault.HasCode(err, ErrorCodeDeliveryFailed) {
			t.Fatalf("Deliver error = %v, want %s", err, ErrorCodeDeliveryFailed)
		}
		if got := h.api.count("deleteMessage"); got != 0 {
			t.Fatalf("deleteMessage calls = %d, want 0 after failed final send", got)
		}
		if states := operationStates(t, h, opDeleteMessage); len(states) != 0 {
			t.Errorf("delete intents = %v, want none", states)
		}
		endPresence(t, adapter, "turn-failed-final")
	})
}

func TestRepeatedDeliverDeletesOnce(t *testing.T) {
	h := newHarness(t)
	adapter := h.adapter(t, nil)
	useInMemoryBotAPI(adapter, h.api)
	ref := replyReference{ChatID: ownerUserID, MessageID: 100, ChatType: chatTypePrivate, FromID: ownerUserID}
	presenceReq := statusTurnRequest(t, h, "turn-repeat-del", ref, "")
	deliverReq := statusTurnRequest(t, h, "turn-repeat-del", ref, "final answer")

	synctest.Test(t, func(t *testing.T) {
		if err := adapter.PresenceStarted(t.Context(), presenceReq); err != nil {
			t.Fatalf("PresenceStarted: %v", err)
		}
		time.Sleep(statusPostAfter)
		synctest.Wait()

		if _, err := adapter.Deliver(t.Context(), deliverReq); err != nil {
			t.Fatalf("first Deliver: %v", err)
		}
		if _, err := adapter.Deliver(t.Context(), deliverReq); err != nil {
			t.Fatalf("second Deliver: %v", err)
		}
		if got := h.api.count("deleteMessage"); got != 1 {
			t.Fatalf("deleteMessage calls = %d, want 1", got)
		}
		endPresence(t, adapter, "turn-repeat-del")
	})
}

func TestQuoteRule(t *testing.T) {
	cases := []struct {
		name      string
		ref       replyReference
		setup     func(t *testing.T, adapter *Adapter)
		wantQuote bool
	}{
		{
			name:      "private chat without newer message",
			ref:       replyReference{ChatID: ownerUserID, MessageID: 100, ChatType: chatTypePrivate, FromID: ownerUserID},
			setup:     func(t *testing.T, a *Adapter) { a.latestPrivate.record(ownerUserID, ownerUserID, 100) },
			wantQuote: false,
		},
		{
			name: "private chat with newer user message",
			ref:  replyReference{ChatID: ownerUserID, MessageID: 100, ChatType: chatTypePrivate, FromID: ownerUserID},
			setup: func(t *testing.T, a *Adapter) {
				a.latestPrivate.record(ownerUserID, ownerUserID, 100)
				a.latestPrivate.record(ownerUserID, ownerUserID, 101)
			},
			wantQuote: true,
		},
		{
			name:      "private chat after restart",
			ref:       replyReference{ChatID: ownerUserID, MessageID: 100, ChatType: chatTypePrivate, FromID: ownerUserID},
			wantQuote: false,
		},
		{
			name:      "group chat quotes trigger",
			ref:       replyReference{ChatID: groupChatID, MessageID: 100, ChatType: chatTypeGroup, FromID: ownerUserID},
			wantQuote: true,
		},
		{
			name:      "supergroup chat quotes trigger",
			ref:       replyReference{ChatID: groupChatID, MessageID: 100, ChatType: chatTypeSupergroup, ThreadID: 77, FromID: ownerUserID},
			wantQuote: true,
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			adapter := h.adapter(t, nil)
			if tc.setup != nil {
				tc.setup(t, adapter)
			}
			req := statusTurnRequest(t, h, "turn-quote-"+strconv.Itoa(i), tc.ref, "reply text")
			if _, err := adapter.Deliver(t.Context(), req); err != nil {
				t.Fatalf("Deliver: %v", err)
			}
			sends := h.api.sends()
			if len(sends) != 1 {
				t.Fatalf("sendMessage calls = %d, want 1", len(sends))
			}
			reply, hasReply := sends[0]["reply_parameters"].(map[string]any)
			if hasReply != tc.wantQuote {
				t.Fatalf("reply_parameters present = %v (%v), want %v", hasReply, sends[0]["reply_parameters"], tc.wantQuote)
			}
			if tc.wantQuote && reply["message_id"] != float64(tc.ref.MessageID) {
				t.Errorf("reply_parameters.message_id = %v, want %d", reply["message_id"], tc.ref.MessageID)
			}
		})
	}

	t.Run("private command reply follows quote rule", func(t *testing.T) {
		rig := newCommandRig(t, accessOpts{})
		rig.handle(t, privateText(10, ownerUserID, "/status"))
		sends := rig.h.api.sends()
		if len(sends) != 1 {
			t.Fatalf("sends = %d, want 1", len(sends))
		}
		if _, hasReply := sends[0]["reply_parameters"]; hasReply {
			t.Errorf("private command reply quoted without newer message: %v", sends[0]["reply_parameters"])
		}

		rig.adapter.latestPrivate.record(ownerUserID, ownerUserID, 101)
		if err := rig.adapter.replyCommand(t.Context(), &message{
			MessageID: 100,
			From:      &user{ID: ownerUserID},
			Chat:      chat{ID: ownerUserID, Type: chatTypePrivate},
		}, "cmd-intent-newer", deliverySession, []string{"ok"}); err != nil {
			t.Fatalf("replyCommand: %v", err)
		}
		sends = rig.h.api.sends()
		reply, hasReply := sends[1]["reply_parameters"].(map[string]any)
		if !hasReply || reply["message_id"] != float64(100) {
			t.Errorf("private command reply after newer message = %v, want quote of 100", sends[1]["reply_parameters"])
		}
	})

	t.Run("group command reply quotes trigger", func(t *testing.T) {
		rig := newCommandRig(t, accessOpts{chats: groupChats})
		rig.knowBot()
		item := commandEntity(groupMessage(10, ownerUserID, "/status@"+testBotName), "/status@"+testBotName)
		rig.handle(t, item)
		sends := rig.h.api.sends()
		if len(sends) != 1 {
			t.Fatalf("sends = %d, want 1", len(sends))
		}
		reply, hasReply := sends[0]["reply_parameters"].(map[string]any)
		if !hasReply || reply["message_id"] != float64(100) {
			t.Errorf("group command reply = %v, want quote of 100", sends[0]["reply_parameters"])
		}
	})

	t.Run("admit records chat_type and newer inbound message", func(t *testing.T) {
		h := newHarness(t)
		adapter := h.adapter(t, nil)
		first := privateText(10, ownerUserID, "first")
		second := privateText(11, ownerUserID, "second")
		env, _, ok := adapter.admit(t.Context(), &first)
		if !ok {
			t.Fatal("first message was not admitted")
		}
		var ref replyReference
		if err := json.Unmarshal(env.ReplyContext, &ref); err != nil {
			t.Fatalf("unmarshal reply context: %v", err)
		}
		if ref.ChatType != chatTypePrivate {
			t.Errorf("replyReference.ChatType = %q, want %q", ref.ChatType, chatTypePrivate)
		}
		if _, _, ok := adapter.admit(t.Context(), &second); !ok {
			t.Fatal("second message was not admitted")
		}
		req := statusTurnRequest(t, h, "turn-admit-quote", ref, "answer to first")
		if _, err := adapter.Deliver(t.Context(), req); err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		sends := h.api.sends()
		reply, hasReply := sends[0]["reply_parameters"].(map[string]any)
		if !hasReply || reply["message_id"] != float64(first.Message.MessageID) {
			t.Errorf("reply_parameters = %v, want quote of %d", sends[0]["reply_parameters"], first.Message.MessageID)
		}
	})

	t.Run("bounded tracker evicts oldest past 1024", func(t *testing.T) {
		var tracker latestInbound
		for id := int64(1); id <= maxTrackedPrivateChats; id++ {
			tracker.record(id, id, 200)
		}
		if !tracker.hasNewer(1, 1, 100) {
			t.Fatal("chat 1 should be tracked before eviction")
		}
		tracker.record(maxTrackedPrivateChats+1, maxTrackedPrivateChats+1, 200)
		if tracker.hasNewer(1, 1, 100) {
			t.Fatal("chat 1 was not evicted when capacity was exceeded")
		}
		if !tracker.hasNewer(2, 2, 100) || !tracker.hasNewer(maxTrackedPrivateChats+1, maxTrackedPrivateChats+1, 100) {
			t.Fatal("remaining chats should stay tracked")
		}
	})
}

func TestStoppedTurnKeepsItsMarker(t *testing.T) {
	h := newHarness(t)
	adapter := h.adapter(t, nil)
	useInMemoryBotAPI(adapter, h.api)
	ref := replyReference{ChatID: ownerUserID, MessageID: 100, ChatType: chatTypePrivate, FromID: ownerUserID}
	presenceReq := statusTurnRequest(t, h, "turn-stopped-marker", ref, "")

	synctest.Test(t, func(t *testing.T) {
		if err := adapter.PresenceStarted(t.Context(), presenceReq); err != nil {
			t.Fatalf("PresenceStarted: %v", err)
		}
		time.Sleep(statusPostAfter)
		synctest.Wait()

		if err := adapter.PresenceEnded(t.Context(), "turn-stopped-marker", runtimechannelhost.PresenceEnd{Stopped: true}); err != nil {
			t.Fatalf("PresenceEnded: %v", err)
		}
		edits := h.api.bodies("editMessageText")
		if len(edits) != 1 || edits[0]["text"] != status.DefaultTexts().Stopped {
			t.Fatalf("edits = %v, want one %q edit", edits, status.DefaultTexts().Stopped)
		}
		if got := h.api.count("deleteMessage"); got != 0 {
			t.Fatalf("deleteMessage calls = %d, want 0 for stopped turn", got)
		}
	})
}
