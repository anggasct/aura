package telegram

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/anggasct/aura/internal/effect"
	runtimechannelhost "github.com/anggasct/aura/internal/runtime/channelhost"
)

const deliverySession = "main/telegram:pocket/chat:424242"

func deliveryRequest(t *testing.T, h *harness, effectID, text string) *runtimechannelhost.DeliveryRequest {
	t.Helper()
	if err := h.sessions.EnsureSession(t.Context(), deliverySession, "local-owner", testPersona); err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	reference, err := json.Marshal(replyReference{ChatID: ownerUserID, MessageID: 100})
	if err != nil {
		t.Fatalf("marshal reply context: %v", err)
	}
	return &runtimechannelhost.DeliveryRequest{
		EffectID:       effectID,
		Channel:        "telegram:pocket",
		ConversationID: deliverySession,
		ReplyContext:   reference,
		Parts:          []runtimechannelhost.OutputPart{{Text: text}},
		IdempotencyKey: effectID,
	}
}

func intentStates(t *testing.T, h *harness) map[effect.State]int {
	t.Helper()
	rows, err := h.db.QueryContext(t.Context(), `SELECT state FROM effect_intent WHERE provider = 'telegram'`)
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

func TestDeliverSplitsLongReplyAndNeverResendsKnownSuccess(t *testing.T) {
	h := newHarness(t)
	adapter := h.adapter(t, nil)
	text := strings.Repeat("a", 4000) + "\n" + strings.Repeat("😀", 100) + "\nend"
	req := deliveryRequest(t, h, "turn-1", text)
	receipt, err := adapter.Deliver(t.Context(), req)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	sends := h.api.sends()
	if len(sends) != 2 {
		t.Fatalf("sendMessage calls = %d, want 2 chunks", len(sends))
	}
	var rebuilt strings.Builder
	for i, send := range sends {
		chunk, _ := send["text"].(string)
		if units := len(utf16.Encode([]rune(chunk))); units > messageLimit {
			t.Errorf("chunk %d is %d UTF-16 units", i, units)
		}
		if chat, _ := send["chat_id"].(float64); chat != ownerUserID {
			t.Errorf("chunk %d chat = %v", i, send["chat_id"])
		}
		_, replies := send["reply_parameters"]
		if replies != (i == 0) {
			t.Errorf("chunk %d reply_parameters present = %v, want only on the first chunk", i, replies)
		}
		rebuilt.WriteString(chunk)
	}
	if rebuilt.String() != text {
		t.Error("chunks do not reassemble the reply in order")
	}
	if first, _ := sends[0]["reply_parameters"].(map[string]any); first["message_id"] != float64(100) || first["allow_sending_without_reply"] != true {
		t.Errorf("reply_parameters = %v", sends[0]["reply_parameters"])
	}
	if receipt.ProviderID != ProviderName || receipt.ExternalID != "702" {
		t.Errorf("receipt = %+v, want the last chunk's message id", receipt)
	}
	if _, err := adapter.Deliver(t.Context(), req); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if got := len(h.api.sends()); got != 2 {
		t.Fatalf("redelivery sent %d more messages, want none", got-2)
	}
	if states := intentStates(t, h); states[effect.StateSucceeded] != 2 || len(states) != 1 {
		t.Errorf("intent states = %v, want two succeeded", states)
	}
}

func TestDeliverAmbiguousSendBecomesUnknownAndIsNotRetried(t *testing.T) {
	h := newHarness(t)
	adapter := h.adapter(t, nil)
	h.api.failNext(apiFailure(http.StatusBadGateway, 0))
	req := deliveryRequest(t, h, "turn-2", "hello")
	_, err := adapter.Deliver(t.Context(), req)
	if code, ok := CodeOf(err); !ok || code != ErrorCodeDeliveryAmbiguous {
		t.Fatalf("Deliver error = %v, want %s", err, ErrorCodeDeliveryAmbiguous)
	}
	if _, err := adapter.Deliver(t.Context(), req); err == nil {
		t.Fatal("redelivery of an unknown send succeeded")
	}
	if got := len(h.api.sends()); got != 1 {
		t.Fatalf("sendMessage calls = %d, want 1 (no blind retry)", got)
	}
	if states := intentStates(t, h); states[effect.StateUnknown] != 1 {
		t.Errorf("intent states = %v, want one unknown", states)
	}
}

func TestDeliverHonorsRetryAfterThenSucceeds(t *testing.T) {
	h := newHarness(t)
	adapter := h.adapter(t, nil)
	h.api.failNext(apiFailure(http.StatusTooManyRequests, 1))
	started := time.Now()
	if _, err := adapter.Deliver(t.Context(), deliveryRequest(t, h, "turn-3", "hello")); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if elapsed := time.Since(started); elapsed < adapter.rateCap {
		t.Errorf("retried after %v, want the capped retry_after wait", elapsed)
	}
	if got := len(h.api.sends()); got != 2 {
		t.Fatalf("sendMessage calls = %d, want 2", got)
	}
}

func TestDeliverDefinitiveRejectionsFailWithoutRetry(t *testing.T) {
	cases := map[string]struct {
		status int
		code   string
		sends  int
	}{
		"blocked by the user":           {status: http.StatusForbidden, code: "telegram_forbidden", sends: 1},
		"bad request after plain retry": {status: http.StatusBadRequest, code: "telegram_rejected", sends: 2},
		"revoked token":                 {status: http.StatusUnauthorized, code: "telegram_unauthorized", sends: 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			adapter := h.adapter(t, nil)
			for range tc.sends {
				h.api.failNext(apiFailure(tc.status, 0))
			}
			_, err := adapter.Deliver(t.Context(), deliveryRequest(t, h, "turn-4", "hello"))
			if code, ok := CodeOf(err); !ok || code != ErrorCodeDeliveryFailed {
				t.Fatalf("Deliver error = %v, want %s", err, ErrorCodeDeliveryFailed)
			}
			var safe string
			if err := h.db.QueryRowContext(t.Context(), `SELECT safe_error_code FROM effect_intent WHERE provider = 'telegram'`).Scan(&safe); err != nil || safe != tc.code {
				t.Fatalf("safe_error_code = %q (%v), want %q", safe, err, tc.code)
			}
			if got := len(h.api.sends()); got != tc.sends {
				t.Fatalf("sendMessage calls = %d, want %d", got, tc.sends)
			}
		})
	}
}

func TestDeliverSendsEscapedHTMLWithParseMode(t *testing.T) {
	h := newHarness(t)
	adapter := h.adapter(t, nil)
	source := "**Done** with <script>alert(1)</script> & `x<y`"
	if _, err := adapter.Deliver(t.Context(), deliveryRequest(t, h, "turn-html", source)); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	sends := h.api.sends()
	if len(sends) != 1 {
		t.Fatalf("sendMessage calls = %d, want 1", len(sends))
	}
	if sends[0]["parse_mode"] != parseModeHTML {
		t.Errorf("parse_mode = %v, want HTML", sends[0]["parse_mode"])
	}
	want := "<b>Done</b> with &lt;script&gt;alert(1)&lt;/script&gt; &amp; <code>x&lt;y</code>"
	if sends[0]["text"] != want {
		t.Errorf("text = %q, want %q", sends[0]["text"], want)
	}
	var key string
	if err := h.db.QueryRowContext(t.Context(), `SELECT idempotency_key FROM effect_intent WHERE provider = 'telegram'`).Scan(&key); err != nil {
		t.Fatalf("query key: %v", err)
	}
	if want := idempotencyKey("turn-html", ownerUserID, 0, source); key != want {
		t.Errorf("idempotency key = %q, want %q derived from the source text", key, want)
	}
}

func TestDeliverFallsBackToPlainTextWhenEntitiesAreRejected(t *testing.T) {
	h := newHarness(t)
	adapter := h.adapter(t, nil)
	h.api.failNextOn("sendMessage", describedFailure(http.StatusBadRequest, entityParseFailure, 0))
	source := "**bold** and [link](https://example.com/a?b=1&c=2)"
	receipt, err := adapter.Deliver(t.Context(), deliveryRequest(t, h, "turn-plain", source))
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	sends := h.api.sends()
	if len(sends) != 2 {
		t.Fatalf("sendMessage calls = %d, want the formatted attempt and one plain resend", len(sends))
	}
	if sends[0]["parse_mode"] != parseModeHTML {
		t.Errorf("first attempt parse_mode = %v, want HTML", sends[0]["parse_mode"])
	}
	if _, formatted := sends[1]["parse_mode"]; formatted || sends[1]["text"] != source {
		t.Errorf("fallback = %v, want the original text without parse_mode", sends[1])
	}
	if _, replies := sends[1]["reply_parameters"]; !replies {
		t.Error("fallback dropped the reply reference")
	}
	if receipt.ExternalID == "" {
		t.Error("fallback receipt carries no message id")
	}
	if states := intentStates(t, h); states[effect.StateSucceeded] != 1 || len(states) != 1 {
		t.Errorf("intent states = %v, want one succeeded intent", states)
	}
}

func TestDeliverSplitsFencedCodeIntoBalancedFormattedChunks(t *testing.T) {
	h := newHarness(t)
	adapter := h.adapter(t, nil)
	text := "Here is the file:\n```go\n" + strings.Repeat("if a < b && c > d { return }\n", 300) + "```\nDone."
	if _, err := adapter.Deliver(t.Context(), deliveryRequest(t, h, "turn-fence", text)); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	sends := h.api.sends()
	if len(sends) < 3 {
		t.Fatalf("sendMessage calls = %d, want the fenced block split across chunks", len(sends))
	}
	for i, send := range sends {
		if send["parse_mode"] != parseModeHTML {
			t.Errorf("chunk %d parse_mode = %v, want HTML (no plain fallback needed)", i, send["parse_mode"])
		}
		markup, _ := send["text"].(string)
		if !strings.Contains(markup, `<pre><code class="language-go">`) {
			t.Errorf("chunk %d lost the code block: %.80q", i, markup)
		}
	}
}

func TestDeliverRejectsRequestsWithoutAChat(t *testing.T) {
	h := newHarness(t)
	adapter := h.adapter(t, nil)
	req := deliveryRequest(t, h, "turn-5", "hello")
	req.ReplyContext = json.RawMessage(`{"message_id":1}`)
	if _, err := adapter.Deliver(t.Context(), req); err == nil {
		t.Fatal("delivery without a chat id accepted")
	}
	req = deliveryRequest(t, h, "turn-5", "  ")
	if _, err := adapter.Deliver(t.Context(), req); err == nil {
		t.Fatal("blank delivery accepted")
	}
	if got := len(h.api.sends()); got != 0 {
		t.Fatalf("sendMessage calls = %d, want 0", got)
	}
}

func TestSplitMessageCountsUTF16AndKeepsOrder(t *testing.T) {
	cases := map[string]string{
		"short":            "hello",
		"exact limit":      strings.Repeat("x", messageLimit),
		"one over":         strings.Repeat("x", messageLimit+1),
		"surrogate pairs":  strings.Repeat("😀", messageLimit/2+3),
		"pair at boundary": strings.Repeat("x", messageLimit-1) + "😀tail",
		"many lines":       strings.Repeat("line of text\n", 1000),
		"long line inside": "head\n" + strings.Repeat("y", 2*messageLimit+5) + "\ntail",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			chunks := splitMessage(text)
			if strings.Join(chunks, "") != text {
				t.Fatal("chunks do not reassemble the input")
			}
			for i, chunk := range chunks {
				if units := len(utf16.Encode([]rune(chunk))); units > messageLimit || units == 0 {
					t.Fatalf("chunk %d has %d UTF-16 units", i, units)
				}
			}
			again := splitMessage(text)
			if strings.Join(again, "\x00") != strings.Join(chunks, "\x00") {
				t.Fatal("split is not deterministic")
			}
		})
	}
	lines := splitMessage(strings.Repeat("abc\n", messageLimit/4) + "next")
	if len(lines) != 2 || lines[1] != "next" {
		t.Fatalf("split did not prefer the line boundary: %d chunks, last %q", len(lines), lines[len(lines)-1])
	}
}
