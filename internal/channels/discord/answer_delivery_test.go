package discord

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/anggasct/aura/internal/channels/kit/status"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/fault"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
)

func (m *memoryDiscordAPI) messageDeletes() []timedRestCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []timedRestCall
	for _, call := range m.calls {
		if call.method == http.MethodDelete {
			out = append(out, call)
		}
	}
	return out
}

func (m *memoryDiscordAPI) allCalls() []timedRestCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]timedRestCall(nil), m.calls...)
}

func discordRefJSON(t *testing.T, ref replyReference) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(ref)
	if err != nil {
		t.Fatalf("marshal reply context: %v", err)
	}
	return raw
}

func TestAnswerIsPostedNewAndTheStatusIsDeleted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		adapter, api := newMemoryDiscordAdapter(t, nil)
		effects, ok := adapter.effects.(*fakeEffectRunner)
		if !ok {
			t.Fatalf("effects = %T", adapter.effects)
		}
		var postSeq atomic.Int32
		baseTransport := adapter.httpClient.Transport
		adapter.httpClient.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/messages") {
				n := postSeq.Add(1)
				id := "status-id"
				if n > 1 {
					id = "answer-id"
				}
				resp, err := baseTransport.RoundTrip(r)
				if err != nil {
					return nil, err
				}
				_ = resp.Body.Close()
				rec := httptest.NewRecorder()
				rec.Header().Set("Content-Type", "application/json")
				_, _ = rec.WriteString(`{"id":"` + id + `","channel_id":"333"}`)
				out := rec.Result()
				out.Request = r
				return out, nil
			}
			if r.Method == http.MethodDelete {
				resp, err := baseTransport.RoundTrip(r)
				if err != nil {
					return nil, err
				}
				_ = resp.Body.Close()
				rec := httptest.NewRecorder()
				rec.WriteHeader(http.StatusNoContent)
				out := rec.Result()
				out.Request = r
				return out, nil
			}
			return baseTransport.RoundTrip(r)
		})

		turnID := "turn-dc-new-del"
		presenceReq := presenceTestRequest(turnID)
		deliverReq := &runtimechannelhost.DeliveryRequest{
			EffectID:       turnID,
			Channel:        "discord:owner",
			ConversationID: "channel:333",
			ReplyContext:   discordRefJSON(t, replyReference{ChannelID: "333", MessageID: "999", GuildID: "222", AuthorID: "111"}),
			Parts:          []runtimechannelhost.OutputPart{{Text: "final answer"}},
			IdempotencyKey: turnID,
		}

		if err := adapter.PresenceStarted(t.Context(), presenceReq); err != nil {
			t.Fatalf("PresenceStarted: %v", err)
		}
		time.Sleep(statusPostAfter)
		synctest.Wait()

		receipt, err := adapter.Deliver(t.Context(), deliverReq)
		if err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		if receipt.ExternalID != "answer-id" {
			t.Errorf("receipt = %+v, want answer-id", receipt)
		}

		posts := api.messagePosts()
		if len(posts) != 2 {
			t.Fatalf("POST /messages calls = %d, want 2 (status and answer)", len(posts))
		}
		var answerPayload map[string]any
		if err := json.Unmarshal(posts[1].body, &answerPayload); err != nil {
			t.Fatalf("unmarshal answer post: %v", err)
		}
		if answerPayload["content"] != "final answer" {
			t.Errorf("answer content = %q, want final answer", answerPayload["content"])
		}
		if edits := api.messageEdits(); len(edits) != 0 {
			t.Fatalf("PATCH calls = %d, want 0", len(edits))
		}
		deletes := api.messageDeletes()
		if len(deletes) != 1 {
			t.Fatalf("DELETE calls = %d, want 1", len(deletes))
		}
		if deletes[0].path != "/api/v10/channels/333/messages/status-id" {
			t.Errorf("DELETE path = %q, want /api/v10/channels/333/messages/status-id", deletes[0].path)
		}

		all := api.allCalls()
		lastPostIdx, deleteIdx := -1, -1
		for i, call := range all {
			if call.method == http.MethodPost && strings.HasSuffix(call.path, "/messages") {
				lastPostIdx = i
			}
			if call.method == http.MethodDelete {
				deleteIdx = i
			}
		}
		if lastPostIdx < 0 || deleteIdx < 0 || deleteIdx < lastPostIdx {
			t.Errorf("call order: last POST at %d, DELETE at %d, want DELETE after final POST", lastPostIdx, deleteIdx)
		}

		effects.mu.Lock()
		var deleteReqs []effect.PrepareRequest
		for _, req := range effects.requests {
			if req.Operation == opDeleteMessage {
				deleteReqs = append(deleteReqs, req)
			}
		}
		effects.mu.Unlock()
		if len(deleteReqs) != 1 {
			t.Fatalf("delete_message effect requests = %d, want 1", len(deleteReqs))
		}
		wantKey := deleteKey(turnID, "status-id")
		if deleteReqs[0].IdempotencyKey != wantKey || deleteReqs[0].Classification != effect.ClassificationEffectful {
			t.Errorf("delete_message req = (%q, %s), want (%q, %s)", deleteReqs[0].IdempotencyKey, deleteReqs[0].Classification, wantKey, effect.ClassificationEffectful)
		}

		if err := adapter.PresenceEnded(t.Context(), turnID, runtimechannelhost.PresenceEnd{}); err != nil {
			t.Fatalf("PresenceEnded: %v", err)
		}
	})
}

func TestStatusDeleteToleratesAlreadyGone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		adapter, api, logs := newStopDiscordAdapter(t, nil)
		effects, ok := adapter.effects.(*fakeEffectRunner)
		if !ok {
			t.Fatalf("effects = %T", adapter.effects)
		}
		baseTransport := adapter.httpClient.Transport
		adapter.httpClient.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodDelete {
				resp, err := baseTransport.RoundTrip(r)
				if err != nil {
					return nil, err
				}
				_ = resp.Body.Close()
				rec := httptest.NewRecorder()
				rec.Header().Set("Content-Type", "application/json")
				rec.WriteHeader(http.StatusNotFound)
				_, _ = rec.WriteString(`{"message":"Unknown Message","code":10008}`)
				out := rec.Result()
				out.Request = r
				return out, nil
			}
			return baseTransport.RoundTrip(r)
		})

		turnID := "turn-dc-already-gone"
		req := presenceTestRequest(turnID)
		deliverReq := &runtimechannelhost.DeliveryRequest{
			EffectID:       turnID,
			Channel:        "discord:owner",
			ConversationID: "channel:333",
			ReplyContext:   req.ReplyContext,
			Parts:          []runtimechannelhost.OutputPart{{Text: "final answer"}},
			IdempotencyKey: turnID,
		}
		if err := adapter.PresenceStarted(t.Context(), req); err != nil {
			t.Fatalf("PresenceStarted: %v", err)
		}
		time.Sleep(statusPostAfter)
		synctest.Wait()

		if _, err := adapter.Deliver(t.Context(), deliverReq); err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		if got := len(api.messageDeletes()); got != 1 {
			t.Fatalf("DELETE calls = %d, want 1", got)
		}
		intentKey := "discord|" + opDeleteMessage + "|" + deleteKey(turnID, "status-id")
		effects.mu.Lock()
		intent := effects.intents[intentKey]
		effects.mu.Unlock()
		if intent == nil || intent.state != effect.StateSucceeded {
			t.Fatalf("delete intent state = %v, want succeeded", intent)
		}
		if strings.Contains(logs.String(), "status delete failed") {
			t.Errorf("unexpected status delete failed log: %s", logs.String())
		}
		if err := adapter.PresenceEnded(t.Context(), turnID, runtimechannelhost.PresenceEnd{}); err != nil {
			t.Fatalf("PresenceEnded: %v", err)
		}
	})
}

func TestStatusDeleteFailureKeepsTheDelivery(t *testing.T) {
	t.Run("rejection", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			adapter, _, logs := newStopDiscordAdapter(t, nil)
			effects, ok := adapter.effects.(*fakeEffectRunner)
			if !ok {
				t.Fatalf("effects = %T", adapter.effects)
			}
			baseTransport := adapter.httpClient.Transport
			adapter.httpClient.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodDelete {
					resp, err := baseTransport.RoundTrip(r)
					if err != nil {
						return nil, err
					}
					_ = resp.Body.Close()
					rec := httptest.NewRecorder()
					rec.WriteHeader(http.StatusForbidden)
					_, _ = rec.WriteString(`{"message":"Missing Permissions","code":50013}`)
					out := rec.Result()
					out.Request = r
					return out, nil
				}
				return baseTransport.RoundTrip(r)
			})

			turnID := "turn-dc-del-reject"
			req := presenceTestRequest(turnID)
			deliverReq := &runtimechannelhost.DeliveryRequest{
				EffectID:       turnID,
				Channel:        "discord:owner",
				ConversationID: "channel:333",
				ReplyContext:   req.ReplyContext,
				Parts:          []runtimechannelhost.OutputPart{{Text: "final answer"}},
				IdempotencyKey: turnID,
			}
			if err := adapter.PresenceStarted(t.Context(), req); err != nil {
				t.Fatalf("PresenceStarted: %v", err)
			}
			time.Sleep(statusPostAfter)
			synctest.Wait()

			receipt, err := adapter.Deliver(t.Context(), deliverReq)
			if err != nil {
				t.Fatalf("Deliver failed on delete rejection: %v", err)
			}
			if receipt.ExternalID == "" {
				t.Error("expected delivery receipt")
			}
			intentKey := "discord|" + opDeleteMessage + "|" + deleteKey(turnID, "status-id")
			effects.mu.Lock()
			intent := effects.intents[intentKey]
			effects.mu.Unlock()
			if intent == nil || intent.state != effect.StateFailed {
				t.Fatalf("delete intent state = %v, want failed", intent)
			}
			if !strings.Contains(logs.String(), "status delete failed") {
				t.Errorf("logs = %s, want status delete failed", logs.String())
			}
			if err := adapter.PresenceEnded(t.Context(), turnID, runtimechannelhost.PresenceEnd{}); err != nil {
				t.Fatalf("PresenceEnded: %v", err)
			}
		})
	})

	t.Run("not found with non-10008 code is failure", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			adapter, _, logs := newStopDiscordAdapter(t, nil)
			effects, ok := adapter.effects.(*fakeEffectRunner)
			if !ok {
				t.Fatalf("effects = %T", adapter.effects)
			}
			baseTransport := adapter.httpClient.Transport
			adapter.httpClient.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodDelete {
					resp, err := baseTransport.RoundTrip(r)
					if err != nil {
						return nil, err
					}
					_ = resp.Body.Close()
					rec := httptest.NewRecorder()
					rec.WriteHeader(http.StatusNotFound)
					_, _ = rec.WriteString(`{"message":"Unknown Channel","code":10003}`)
					out := rec.Result()
					out.Request = r
					return out, nil
				}
				return baseTransport.RoundTrip(r)
			})

			turnID := "turn-dc-del-unknown-channel"
			req := presenceTestRequest(turnID)
			deliverReq := &runtimechannelhost.DeliveryRequest{
				EffectID:       turnID,
				Channel:        "discord:owner",
				ConversationID: "channel:333",
				ReplyContext:   req.ReplyContext,
				Parts:          []runtimechannelhost.OutputPart{{Text: "final answer"}},
				IdempotencyKey: turnID,
			}
			if err := adapter.PresenceStarted(t.Context(), req); err != nil {
				t.Fatalf("PresenceStarted: %v", err)
			}
			time.Sleep(statusPostAfter)
			synctest.Wait()

			if _, err := adapter.Deliver(t.Context(), deliverReq); err != nil {
				t.Fatalf("Deliver: %v", err)
			}
			intentKey := "discord|" + opDeleteMessage + "|" + deleteKey(turnID, "status-id")
			effects.mu.Lock()
			intent := effects.intents[intentKey]
			effects.mu.Unlock()
			if intent == nil || intent.state != effect.StateFailed {
				t.Fatalf("delete intent state = %v, want failed", intent)
			}
			if !strings.Contains(logs.String(), "status delete failed") {
				t.Errorf("logs = %s, want status delete failed", logs.String())
			}
			if err := adapter.PresenceEnded(t.Context(), turnID, runtimechannelhost.PresenceEnd{}); err != nil {
				t.Fatalf("PresenceEnded: %v", err)
			}
		})
	})

	t.Run("timeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			adapter, _, logs := newStopDiscordAdapter(t, nil)
			adapter.deleteTimeout = 20 * time.Millisecond
			effects, ok := adapter.effects.(*fakeEffectRunner)
			if !ok {
				t.Fatalf("effects = %T", adapter.effects)
			}
			baseTransport := adapter.httpClient.Transport
			adapter.httpClient.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodDelete {
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
				return baseTransport.RoundTrip(r)
			})

			turnID := "turn-dc-del-timeout"
			req := presenceTestRequest(turnID)
			deliverReq := &runtimechannelhost.DeliveryRequest{
				EffectID:       turnID,
				Channel:        "discord:owner",
				ConversationID: "channel:333",
				ReplyContext:   req.ReplyContext,
				Parts:          []runtimechannelhost.OutputPart{{Text: "final answer"}},
				IdempotencyKey: turnID,
			}
			if err := adapter.PresenceStarted(t.Context(), req); err != nil {
				t.Fatalf("PresenceStarted: %v", err)
			}
			time.Sleep(statusPostAfter)
			synctest.Wait()

			receipt, err := adapter.Deliver(t.Context(), deliverReq)
			if err != nil {
				t.Fatalf("Deliver failed on delete timeout: %v", err)
			}
			if receipt.ExternalID == "" {
				t.Error("expected delivery receipt")
			}
			intentKey := "discord|" + opDeleteMessage + "|" + deleteKey(turnID, "status-id")
			effects.mu.Lock()
			intent := effects.intents[intentKey]
			effects.mu.Unlock()
			if intent == nil || intent.state != effect.StateUnknown {
				t.Fatalf("delete intent state = %v, want unknown", intent)
			}
			if !strings.Contains(logs.String(), "status delete failed") {
				t.Errorf("logs = %s, want status delete failed", logs.String())
			}
			if err := adapter.PresenceEnded(t.Context(), turnID, runtimechannelhost.PresenceEnd{}); err != nil {
				t.Fatalf("PresenceEnded: %v", err)
			}
		})
	})
}

func TestFailedFinalMessageKeepsTheStatus(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		adapter, api := newMemoryDiscordAdapter(t, nil)
		var postCount atomic.Int32
		baseTransport := adapter.httpClient.Transport
		adapter.httpClient.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/messages") && postCount.Add(1) == 2 {
				rec := httptest.NewRecorder()
				rec.WriteHeader(http.StatusForbidden)
				out := rec.Result()
				out.Request = r
				return out, nil
			}
			return baseTransport.RoundTrip(r)
		})

		turnID := "turn-dc-failed-final"
		req := presenceTestRequest(turnID)
		deliverReq := &runtimechannelhost.DeliveryRequest{
			EffectID:       turnID,
			Channel:        "discord:owner",
			ConversationID: "channel:333",
			ReplyContext:   req.ReplyContext,
			Parts:          []runtimechannelhost.OutputPart{{Text: "final answer"}},
			IdempotencyKey: turnID,
		}
		if err := adapter.PresenceStarted(t.Context(), req); err != nil {
			t.Fatalf("PresenceStarted: %v", err)
		}
		time.Sleep(statusPostAfter)
		synctest.Wait()

		_, err := adapter.Deliver(t.Context(), deliverReq)
		if !fault.HasCode(err, ErrorCodeDeliveryFailed) {
			t.Fatalf("Deliver error = %v, want %s", err, ErrorCodeDeliveryFailed)
		}
		if got := len(api.messageDeletes()); got != 0 {
			t.Fatalf("DELETE calls = %d, want 0 after failed final send", got)
		}
		if err := adapter.PresenceEnded(t.Context(), turnID, runtimechannelhost.PresenceEnd{}); err != nil {
			t.Fatalf("PresenceEnded: %v", err)
		}
	})
}

func TestRepeatedDeliverDeletesOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		adapter, api := newMemoryDiscordAdapter(t, func(cfg *Config) {
			cfg.MinEditInterval = 0
		})
		turnID := "turn-dc-repeat-del"
		req := presenceTestRequest(turnID)
		deliverReq := &runtimechannelhost.DeliveryRequest{
			EffectID:       turnID,
			Channel:        "discord:owner",
			ConversationID: "channel:333",
			ReplyContext:   req.ReplyContext,
			Parts:          []runtimechannelhost.OutputPart{{Text: "final answer"}},
			IdempotencyKey: turnID,
		}
		if err := adapter.PresenceStarted(t.Context(), req); err != nil {
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
		if got := len(api.messageDeletes()); got != 1 {
			t.Fatalf("DELETE calls = %d, want 1", got)
		}
		if err := adapter.PresenceEnded(t.Context(), turnID, runtimechannelhost.PresenceEnd{}); err != nil {
			t.Fatalf("PresenceEnded: %v", err)
		}
	})
}

func TestQuoteRule(t *testing.T) {
	cases := []struct {
		name      string
		ref       replyReference
		setup     func(a *Adapter)
		wantQuote bool
	}{
		{
			name:      "DM without newer message",
			ref:       replyReference{ChannelID: "555", MessageID: "100", AuthorID: "111"},
			setup:     func(a *Adapter) { a.latestDM.record("555", "111", "100") },
			wantQuote: false,
		},
		{
			name: "DM with newer user message",
			ref:  replyReference{ChannelID: "555", MessageID: "100", AuthorID: "111"},
			setup: func(a *Adapter) {
				a.latestDM.record("555", "111", "100")
				a.latestDM.record("555", "111", "101")
			},
			wantQuote: true,
		},
		{
			name:      "DM after restart",
			ref:       replyReference{ChannelID: "555", MessageID: "100", AuthorID: "111"},
			wantQuote: false,
		},
		{
			name:      "guild channel quotes trigger",
			ref:       replyReference{ChannelID: "333", MessageID: "100", GuildID: "222", AuthorID: "111"},
			wantQuote: true,
		},
		{
			name:      "bot thread does not quote",
			ref:       replyReference{ChannelID: "333", ThreadID: "777", MessageID: "100", GuildID: "222", AuthorID: "111"},
			wantQuote: false,
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newRestStub(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				writeMessage(t, w, "m1")
			})
			adapter, _ := newSendTestAdapter(t, stub, nil)
			if tc.setup != nil {
				tc.setup(adapter)
			}
			req := &runtimechannelhost.DeliveryRequest{
				EffectID:       "turn-dc-quote-" + strconv.Itoa(i),
				Channel:        "discord:owner",
				ConversationID: "channel:" + tc.ref.ChannelID,
				ReplyContext:   discordRefJSON(t, tc.ref),
				Parts:          []runtimechannelhost.OutputPart{{Text: "reply text"}},
				IdempotencyKey: "key-" + strconv.Itoa(i),
			}
			if _, err := adapter.Deliver(t.Context(), req); err != nil {
				t.Fatalf("Deliver: %v", err)
			}
			sends := stub.messageSends(0)
			if len(sends) != 1 {
				t.Fatalf("POST /messages calls = %d, want 1", len(sends))
			}
			var body map[string]any
			if err := json.Unmarshal(sends[0].body, &body); err != nil {
				t.Fatalf("unmarshal send body: %v", err)
			}
			ref, hasRef := body["message_reference"].(map[string]any)
			if hasRef != tc.wantQuote {
				t.Fatalf("message_reference present = %v (%v), want %v", hasRef, body["message_reference"], tc.wantQuote)
			}
			if tc.wantQuote && ref["message_id"] != tc.ref.MessageID {
				t.Errorf("message_reference.message_id = %v, want %q", ref["message_id"], tc.ref.MessageID)
			}
		})
	}

	t.Run("command replies follow quote rule", func(t *testing.T) {
		f := newCommandFixture(t)
		f.admit(t, dmMessage("8001", "111", "/help"))
		replies := f.replies(t)
		if len(replies) != 1 || replies[0].replyTo != "" {
			t.Fatalf("DM command reply without newer message = %+v, want no quote", replies)
		}

		f.adapter.latestDM.record("555", "111", "8002")
		if err := f.adapter.replyCommand(
			t.Context(),
			discordRefJSON(t, replyReference{ChannelID: "555", MessageID: "8001", AuthorID: "111"}),
			"cmd-dm-newer",
			testSessionKey("dm:111"),
			[]string{"ok"},
		); err != nil {
			t.Fatalf("replyCommand: %v", err)
		}
		replies = f.replies(t)
		if len(replies) != 2 || replies[1].replyTo != "8001" {
			t.Fatalf("DM command reply after newer message = %+v, want quote of 8001", replies)
		}

		f.admit(t, threadTrigger("9100", "<@999> /help", []string{"999"}))
		replies = f.replies(t)
		if len(replies) != 3 || replies[2].replyTo != "9100" {
			t.Fatalf("guild channel command reply = %+v, want quote of 9100", replies)
		}

		f.admit(t, threadTrigger("9200", "<@999> start thread", []string{"999"}))
		f.admit(t, threadFollowUp("9201", "777", "/help"))
		replies = f.replies(t)
		if len(replies) != 4 || replies[3].channel != "777" || replies[3].replyTo != "" {
			t.Fatalf("bot thread command reply = %+v, want no quote in thread 777", replies)
		}
	})

	t.Run("admit records newer DM message", func(t *testing.T) {
		stub := newRestStub(t, func(_ int, w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/typing") {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			writeMessage(t, w, "m1")
		})
		sink := newMemorySink()
		adapter := newThreadAdapter(t, sink, &fakeSessionEnsurer{}, stub)
		adapter.logger = slog.New(slog.DiscardHandler)
		if admitted, err := adapter.admit(t.Context(), dmMessage("100", "111", "first")); err != nil || !admitted {
			t.Fatalf("first admit = %v, %v", admitted, err)
		}
		if admitted, err := adapter.admit(t.Context(), dmMessage("101", "111", "second")); err != nil || !admitted {
			t.Fatalf("second admit = %v, %v", admitted, err)
		}
		firstEnv := sink.envelopes["message:100"]
		req := &runtimechannelhost.DeliveryRequest{
			EffectID:       "turn-dm-admit",
			Channel:        "discord:owner",
			ConversationID: firstEnv.ConversationID,
			ReplyContext:   firstEnv.ReplyContext,
			Parts:          []runtimechannelhost.OutputPart{{Text: "answer to first"}},
			IdempotencyKey: "turn-dm-admit",
		}
		if _, err := adapter.Deliver(t.Context(), req); err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		sends := stub.messageSends(0)
		var body map[string]any
		if err := json.Unmarshal(sends[0].body, &body); err != nil {
			t.Fatalf("unmarshal send body: %v", err)
		}
		ref, hasRef := body["message_reference"].(map[string]any)
		if !hasRef || ref["message_id"] != "100" {
			t.Errorf("message_reference = %v, want quote of 100", body["message_reference"])
		}
	})

	t.Run("bounded tracker evicts oldest past 1024", func(t *testing.T) {
		var tracker latestInbound
		for i := 1; i <= maxTrackedDMs; i++ {
			id := strconv.Itoa(i)
			tracker.record(id, "111", "200")
		}
		if !tracker.hasNewer("1", "111", "100") {
			t.Fatal("channel 1 should be tracked before eviction")
		}
		tracker.record(strconv.Itoa(maxTrackedDMs+1), "111", "200")
		if tracker.hasNewer("1", "111", "100") {
			t.Fatal("channel 1 was not evicted when capacity was exceeded")
		}
		if !tracker.hasNewer("2", "111", "100") || !tracker.hasNewer(strconv.Itoa(maxTrackedDMs+1), "111", "100") {
			t.Fatal("remaining channels should stay tracked")
		}
	})
}

func TestStoppedTurnKeepsItsMarker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		adapter, api := newMemoryDiscordAdapter(t, nil)
		turnID := "turn-dc-stopped-marker"
		req := presenceTestRequest(turnID)
		if err := adapter.PresenceStarted(t.Context(), req); err != nil {
			t.Fatalf("PresenceStarted: %v", err)
		}
		time.Sleep(statusPostAfter)
		synctest.Wait()

		if err := adapter.PresenceEnded(t.Context(), turnID, runtimechannelhost.PresenceEnd{Stopped: true}); err != nil {
			t.Fatalf("PresenceEnded: %v", err)
		}
		edits := api.messageEdits()
		if len(edits) != 1 {
			t.Fatalf("edits = %d, want 1", len(edits))
		}
		var body map[string]any
		if err := json.Unmarshal(edits[0].body, &body); err != nil {
			t.Fatalf("unmarshal edit: %v", err)
		}
		if body["content"] != status.DefaultTexts().Stopped {
			t.Errorf("content = %q, want %q", body["content"], status.DefaultTexts().Stopped)
		}
		if got := len(api.messageDeletes()); got != 0 {
			t.Fatalf("DELETE calls = %d, want 0 for stopped turn", got)
		}
	})
}
