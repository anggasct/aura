package discord

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	runtimechannelhost "github.com/anggasct/aura/internal/runtime/channelhost"
	"github.com/anggasct/aura/internal/secret"
)

func presenceTestRequest(turnID string) *runtimechannelhost.DeliveryRequest {
	return &runtimechannelhost.DeliveryRequest{
		EffectID:       turnID,
		Channel:        "discord:owner",
		ConversationID: "channel:333",
		ReplyContext:   json.RawMessage(`{"channel_id":"333","message_id":"999"}`),
		IdempotencyKey: turnID,
	}
}

func messagePostCount(stub *restStub) int {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	n := 0
	for _, call := range stub.calls {
		if call.method == "POST" && strings.Contains(call.path, "/messages") {
			n++
		}
	}
	return n
}

func TestTriggerTypingPostsTypingCall(t *testing.T) {
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/typing") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeMessage(t, w, "m1")
	})
	adapter, _ := newSendTestAdapter(t, stub, nil)
	if err := adapter.triggerTyping(t.Context(), "333"); err != nil {
		t.Fatalf("triggerTyping(): %v", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	found := false
	for _, call := range stub.calls {
		if call.method == http.MethodPost && strings.HasSuffix(call.path, "/typing") {
			found = true
			if call.auth != "Bot "+testToken {
				t.Error("typing call misses bot authorization")
			}
		}
	}
	if !found {
		t.Fatal("no typing call captured")
	}
}

func TestPresenceFastTurnPostsNothing(t *testing.T) {
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		writeMessage(t, w, "m1")
	})
	adapter, _ := newSendTestAdapter(t, stub, nil)
	adapter.statusAfter = 10 * time.Second
	adapter.typingRefresh = 8 * time.Second
	req := presenceTestRequest("turn-fast-1")
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted(): %v", err)
	}
	if messagePostCount(stub) != 0 {
		t.Fatalf("posts = %d, want 0 for fast turn before status gate", messagePostCount(stub))
	}
	if err := adapter.PresenceEnded(t.Context(), "turn-fast-1"); err != nil {
		t.Fatalf("PresenceEnded(): %v", err)
	}
}

func TestPresencePostOnceEditInPlace(t *testing.T) {
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/typing") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeMessage(t, w, "status-id")
	})
	adapter, _ := newSendTestAdapter(t, stub, func(cfg *config.Discord) {
		cfg.MinEditInterval = 0
	})
	adapter.statusAfter = time.Nanosecond
	adapter.typingRefresh = time.Nanosecond
	req := presenceTestRequest("turn-slow-1")
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted(): %v", err)
	}
	if err := adapter.PresenceUpdated(t.Context(), req, "Looking through notes", false); err != nil {
		t.Fatalf("first PresenceUpdated(): %v", err)
	}
	if err := adapter.PresenceUpdated(t.Context(), req, "Checking open tasks", false); err != nil {
		t.Fatalf("second PresenceUpdated(): %v", err)
	}
	if messagePostCount(stub) != 1 {
		t.Fatalf("posts = %d, want exactly 1 status post", messagePostCount(stub))
	}
	patches := stub.patchBodies()
	if len(patches) != 1 {
		t.Fatalf("patches = %d, want 1 edit in place", len(patches))
	}
	var payload map[string]any
	if err := json.Unmarshal(patches[0], &payload); err != nil {
		t.Fatalf("patch body: %v", err)
	}
	content, _ := payload["content"].(string)
	if !strings.Contains(content, "Checking open tasks") {
		t.Errorf("edited content = %q, want latest activity", content)
	}
}

func TestPresenceSilentFallback(t *testing.T) {
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		writeMessage(t, w, "status-id")
	})
	adapter, _ := newSendTestAdapter(t, stub, func(cfg *config.Discord) {
		cfg.MinEditInterval = 0
	})
	adapter.statusAfter = time.Nanosecond
	req := presenceTestRequest("turn-silent-1")
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted(): %v", err)
	}
	if err := adapter.PresenceUpdated(t.Context(), req, "", false); err != nil {
		t.Fatalf("PresenceUpdated(): %v", err)
	}
	if messagePostCount(stub) != 1 {
		t.Fatalf("posts = %d, want 1 fallback post", messagePostCount(stub))
	}
	stub.mu.Lock()
	body := ""
	for _, call := range stub.calls {
		if call.method == "POST" && strings.Contains(call.path, "/messages") {
			body = string(call.body)
		}
	}
	stub.mu.Unlock()
	matched := false
	for _, fallback := range fallbackStatuses {
		if strings.Contains(body, fallback) {
			matched = true
		}
	}
	if !matched {
		t.Errorf("fallback body = %q, want one of the generic statuses", body)
	}
}

func TestPresenceRedactsSecrets(t *testing.T) {
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		writeMessage(t, w, "status-id")
	})
	adapter, _ := newSendTestAdapter(t, stub, func(cfg *config.Discord) {
		cfg.MinEditInterval = 0
	})
	adapter.statusAfter = time.Nanosecond
	vault := secret.New()
	vault.Set("probe", "hunter2-secret-value")
	adapter.SetSecretVault(vault)
	req := presenceTestRequest("turn-secret-1")
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted(): %v", err)
	}
	if err := adapter.PresenceUpdated(t.Context(), req, "Checking hunter2-secret-value", false); err != nil {
		t.Fatalf("PresenceUpdated(): %v", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	for _, call := range stub.calls {
		if strings.Contains(string(call.body), "hunter2-secret-value") {
			t.Fatal("secret value leaked into outbound body")
		}
	}
}

func TestPresenceFailureLine(t *testing.T) {
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		writeMessage(t, w, "status-id")
	})
	adapter, _ := newSendTestAdapter(t, stub, func(cfg *config.Discord) {
		cfg.MinEditInterval = 0
	})
	adapter.statusAfter = time.Nanosecond
	req := presenceTestRequest("turn-fail-1")
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted(): %v", err)
	}
	if err := adapter.PresenceUpdated(t.Context(), req, "Looking through notes", false); err != nil {
		t.Fatalf("activity update: %v", err)
	}
	if err := adapter.PresenceUpdated(t.Context(), req, "", true); err != nil {
		t.Fatalf("failure update: %v", err)
	}
	patches := stub.patchBodies()
	if len(patches) == 0 {
		t.Fatal("failure produced no edit")
	}
	var payload map[string]any
	_ = json.Unmarshal(patches[len(patches)-1], &payload)
	content, _ := payload["content"].(string)
	if !strings.Contains(content, "Looking through notes") {
		t.Errorf("failure line = %q, want last activity named", content)
	}
	if strings.Contains(strings.ToLower(content), "read_file") || strings.Contains(content, "trace") {
		t.Errorf("failure line leaks internals: %q", content)
	}
}

func TestPresenceThrottleCoalesces(t *testing.T) {
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		writeMessage(t, w, "status-id")
	})
	adapter, _ := newSendTestAdapter(t, stub, func(cfg *config.Discord) {
		cfg.MinEditInterval = 3600000000000
	})
	adapter.statusAfter = time.Nanosecond
	req := presenceTestRequest("turn-throttle-1")
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted(): %v", err)
	}
	if err := adapter.PresenceUpdated(t.Context(), req, "First activity here", false); err != nil {
		t.Fatalf("first update: %v", err)
	}
	if err := adapter.PresenceUpdated(t.Context(), req, "Second activity here", false); err != nil {
		t.Fatalf("coalesced update returned error")
	}
	if patches := stub.patchBodies(); len(patches) != 0 {
		t.Fatalf("patches = %d, want 0 while throttled", len(patches))
	}
}

func TestPresenceStatusReplacedByFinalDelivery(t *testing.T) {
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/typing") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPatch {
			writeMessage(t, w, "status-id")
			return
		}
		writeMessage(t, w, "status-id")
	})
	adapter, _ := newSendTestAdapter(t, stub, func(cfg *config.Discord) {
		cfg.MinEditInterval = 0
	})
	adapter.statusAfter = time.Nanosecond
	adapter.typingRefresh = time.Nanosecond
	turnID := "turn-replace-1"
	req := presenceTestRequest(turnID)
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted(): %v", err)
	}
	if err := adapter.PresenceUpdated(t.Context(), req, "Looking through notes", false); err != nil {
		t.Fatalf("PresenceUpdated(): %v", err)
	}
	if messagePostCount(stub) != 1 {
		t.Fatalf("status posts = %d, want 1", messagePostCount(stub))
	}
	deliverReq := &runtimechannelhost.DeliveryRequest{
		EffectID:       turnID,
		Channel:        "discord:owner",
		ConversationID: "channel:333",
		ReplyContext:   json.RawMessage(`{"channel_id":"333","message_id":"999"}`),
		Parts:          []runtimechannelhost.OutputPart{{Text: "final answer"}},
		IdempotencyKey: turnID,
	}
	if _, err := adapter.Deliver(t.Context(), deliverReq); err != nil {
		t.Fatalf("Deliver(): %v", err)
	}
	if messagePostCount(stub) != 1 {
		t.Fatalf("message posts = %d, want still 1 (final edits status)", messagePostCount(stub))
	}
	patches := stub.patchBodies()
	if len(patches) == 0 {
		t.Fatal("final delivery produced no edit")
	}
	var payload map[string]any
	_ = json.Unmarshal(patches[len(patches)-1], &payload)
	content, _ := payload["content"].(string)
	if !strings.Contains(content, "final answer") {
		t.Errorf("final content = %q, want answer replacing status", content)
	}
}

func TestRenderStatusHelpers(t *testing.T) {
	if got := truncateWords("one two three four five six seven", 6); got != "one two three four five six" {
		t.Errorf("truncateWords = %q", got)
	}
	if got := fallbackStatus(0); got == "" {
		t.Error("empty fallback")
	}
	if failureLine("") == "" || !strings.Contains(failureLine(""), "continuing") {
		t.Error("empty failure line misses continuation")
	}
	rendered := renderStatusText("Looking through notes", `{"path":"secret-token abc123"}`, 0, nil)
	if strings.Contains(strings.ToLower(rendered), "secret-token") && strings.Contains(rendered, "abc123") {
		t.Errorf("keyword-bearing args were not omitted: %q", rendered)
	}
	human := humanizeArgs(`{"path":"src/main.go","query":"hello"}`)
	if strings.Contains(human, "{") || strings.Contains(human, `"`) {
		t.Errorf("humanized args leak JSON: %q", human)
	}
	if !strings.Contains(human, "src/main.go") {
		t.Errorf("humanized args = %q, want values preserved", human)
	}
}
