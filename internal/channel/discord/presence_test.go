package discord

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
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

func typingCallCount(stub *restStub) int {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	n := 0
	for _, call := range stub.calls {
		if call.method == http.MethodPost && strings.HasSuffix(call.path, "/typing") {
			n++
		}
	}
	return n
}

func outboundBodies(stub *restStub) []string {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	bodies := make([]string, 0, len(stub.calls))
	for _, call := range stub.calls {
		if call.method == http.MethodPost || call.method == http.MethodPatch {
			bodies = append(bodies, string(call.body))
		}
	}
	return bodies
}

func waitForStatusPost(t *testing.T, stub *restStub) {
	t.Helper()
	waitFor(t, 2*time.Second, func() bool { return messagePostCount(stub) == 1 }, "status post")
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
	adapter.statusAfter = 50 * time.Millisecond
	adapter.typingRefresh = 20 * time.Millisecond
	req := presenceTestRequest("turn-slow-1")
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted(): %v", err)
	}
	if err := adapter.PresenceUpdated(t.Context(), req, "Looking through notes", "", false); err != nil {
		t.Fatalf("first PresenceUpdated(): %v", err)
	}
	waitForStatusPost(t, stub)
	if err := adapter.PresenceUpdated(t.Context(), req, "Checking open tasks", "", false); err != nil {
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
	if err := adapter.PresenceEnded(t.Context(), "turn-slow-1"); err != nil {
		t.Fatalf("PresenceEnded(): %v", err)
	}
}

func TestPresenceSilentFallback(t *testing.T) {
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		writeMessage(t, w, "status-id")
	})
	adapter, _ := newSendTestAdapter(t, stub, func(cfg *config.Discord) {
		cfg.MinEditInterval = 0
	})
	adapter.statusAfter = 30 * time.Millisecond
	req := presenceTestRequest("turn-silent-1")
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted(): %v", err)
	}
	waitForStatusPost(t, stub)
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
	if err := adapter.PresenceEnded(t.Context(), "turn-silent-1"); err != nil {
		t.Fatalf("PresenceEnded(): %v", err)
	}
}

func TestPresenceRedactsSecrets(t *testing.T) {
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		writeMessage(t, w, "status-id")
	})
	adapter, _ := newSendTestAdapter(t, stub, func(cfg *config.Discord) {
		cfg.MinEditInterval = 0
	})
	adapter.statusAfter = 30 * time.Millisecond
	vault := secret.New()
	vault.Set("probe", "hunter2-secret-value")
	adapter.SetSecretVault(vault)
	req := presenceTestRequest("turn-secret-1")
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted(): %v", err)
	}
	if err := adapter.PresenceUpdated(t.Context(), req, "Checking hunter2-secret-value", "", false); err != nil {
		t.Fatalf("PresenceUpdated(): %v", err)
	}
	waitForStatusPost(t, stub)
	stub.mu.Lock()
	defer stub.mu.Unlock()
	for _, call := range stub.calls {
		if strings.Contains(string(call.body), "hunter2-secret-value") {
			t.Fatal("secret value leaked into outbound body")
		}
	}
	if err := adapter.PresenceEnded(t.Context(), "turn-secret-1"); err != nil {
		t.Fatalf("PresenceEnded(): %v", err)
	}
}

func TestPresenceFailureLine(t *testing.T) {
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		writeMessage(t, w, "status-id")
	})
	adapter, _ := newSendTestAdapter(t, stub, func(cfg *config.Discord) {
		cfg.MinEditInterval = 0
	})
	adapter.statusAfter = 30 * time.Millisecond
	req := presenceTestRequest("turn-fail-1")
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted(): %v", err)
	}
	if err := adapter.PresenceUpdated(t.Context(), req, "Looking through notes", "", false); err != nil {
		t.Fatalf("activity update: %v", err)
	}
	waitForStatusPost(t, stub)
	if err := adapter.PresenceUpdated(t.Context(), req, "", "", true); err != nil {
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
	if err := adapter.PresenceEnded(t.Context(), "turn-fail-1"); err != nil {
		t.Fatalf("PresenceEnded(): %v", err)
	}
}

func TestPresenceRepeatedFailureCoalesced(t *testing.T) {
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		writeMessage(t, w, "status-id")
	})
	adapter, _ := newSendTestAdapter(t, stub, func(cfg *config.Discord) {
		cfg.MinEditInterval = 0
	})
	adapter.statusAfter = 30 * time.Millisecond
	req := presenceTestRequest("turn-fail-coalesce")
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted(): %v", err)
	}
	if err := adapter.PresenceUpdated(t.Context(), req, "Checking tasks", "", false); err != nil {
		t.Fatalf("activity update: %v", err)
	}
	waitForStatusPost(t, stub)
	if err := adapter.PresenceUpdated(t.Context(), req, "", "", true); err != nil {
		t.Fatalf("first failure update: %v", err)
	}
	if err := adapter.PresenceUpdated(t.Context(), req, "", "", true); err != nil {
		t.Fatalf("second failure update: %v", err)
	}
	if err := adapter.PresenceUpdated(t.Context(), req, "", "", true); err != nil {
		t.Fatalf("third failure update: %v", err)
	}
	patches := stub.patchBodies()
	if len(patches) != 1 {
		t.Fatalf("patches = %d, want 1 (single failure edit, repeated failures coalesced)", len(patches))
	}
	var payload map[string]any
	_ = json.Unmarshal(patches[len(patches)-1], &payload)
	content, _ := payload["content"].(string)
	if !strings.Contains(content, "Checking tasks") {
		t.Errorf("failure line = %q, want activity named", content)
	}
	count := strings.Count(strings.ToLower(content), "couldn't finish")
	if count != 1 {
		t.Errorf("failure line wrapped multiple times: %q", content)
	}
	if err := adapter.PresenceEnded(t.Context(), "turn-fail-coalesce"); err != nil {
		t.Fatalf("PresenceEnded(): %v", err)
	}
}

func TestPresenceThrottleCoalesces(t *testing.T) {
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		writeMessage(t, w, "status-id")
	})
	adapter, _ := newSendTestAdapter(t, stub, func(cfg *config.Discord) {
		cfg.MinEditInterval = 3600000000000
	})
	adapter.statusAfter = 30 * time.Millisecond
	req := presenceTestRequest("turn-throttle-1")
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted(): %v", err)
	}
	if err := adapter.PresenceUpdated(t.Context(), req, "First activity here", "", false); err != nil {
		t.Fatalf("first update: %v", err)
	}
	waitForStatusPost(t, stub)
	if err := adapter.PresenceUpdated(t.Context(), req, "Second activity here", "", false); err != nil {
		t.Fatalf("coalesced update returned error")
	}
	if patches := stub.patchBodies(); len(patches) != 0 {
		t.Fatalf("patches = %d, want 0 while throttled", len(patches))
	}
	if err := adapter.PresenceEnded(t.Context(), "turn-throttle-1"); err != nil {
		t.Fatalf("PresenceEnded(): %v", err)
	}
}

func TestPresenceThrottleCoalescesInFlightPost(t *testing.T) {
	postStarted := make(chan struct{})
	releasePost := make(chan struct{})
	var messagePosts atomic.Int32
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/typing") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/messages") {
			if messagePosts.Add(1) == 1 {
				close(postStarted)
			}
			<-releasePost
		}
		writeMessage(t, w, "status-id")
	})
	adapter, _ := newSendTestAdapter(t, stub, func(cfg *config.Discord) {
		cfg.MinEditInterval = config.Duration(time.Hour)
	})
	adapter.statusAfter = 10 * time.Millisecond
	adapter.typingRefresh = time.Hour
	turnID := "turn-throttle-inflight"
	req := presenceTestRequest(turnID)
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted(): %v", err)
	}
	select {
	case <-postStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("initial status post did not start")
	}
	updateDone := make(chan error, 1)
	go func() {
		updateDone <- adapter.PresenceUpdated(t.Context(), req, "Second activity here", "", false)
	}()
	time.Sleep(20 * time.Millisecond)
	close(releasePost)
	select {
	case err := <-updateDone:
		if err != nil {
			t.Fatalf("throttled update: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("throttled update did not return")
	}
	if patches := stub.patchBodies(); len(patches) != 0 {
		t.Fatalf("patches = %d, want 0 while throttled", len(patches))
	}
	if err := adapter.PresenceEnded(t.Context(), turnID); err != nil {
		t.Fatalf("PresenceEnded(): %v", err)
	}
}

func TestPresenceDeliveryStopsClockBeforeAnswer(t *testing.T) {
	postStarted := make(chan struct{})
	releasePost := make(chan struct{})
	var messagePosts atomic.Int32
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/typing") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/messages") {
			if messagePosts.Add(1) == 1 {
				close(postStarted)
			}
			<-releasePost
		}
		writeMessage(t, w, "status-id")
	})
	adapter, _ := newSendTestAdapter(t, stub, func(cfg *config.Discord) {
		cfg.MinEditInterval = 0
	})
	adapter.statusAfter = 10 * time.Millisecond
	adapter.typingRefresh = time.Hour
	turnID := "turn-deliver-inflight"
	req := presenceTestRequest(turnID)
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted(): %v", err)
	}
	select {
	case <-postStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("initial status post did not start")
	}
	deliverReq := &runtimechannelhost.DeliveryRequest{
		EffectID:       turnID,
		Channel:        "discord:owner",
		ConversationID: "channel:333",
		ReplyContext:   json.RawMessage(`{"channel_id":"333","message_id":"999"}`),
		Parts:          []runtimechannelhost.OutputPart{{Text: "final answer"}},
		IdempotencyKey: turnID,
	}
	deliverDone := make(chan error, 1)
	go func() {
		_, err := adapter.Deliver(t.Context(), deliverReq)
		deliverDone <- err
	}()
	time.Sleep(50 * time.Millisecond)
	close(releasePost)
	select {
	case err := <-deliverDone:
		if err != nil {
			t.Fatalf("Deliver(): %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Deliver did not finish")
	}
	time.Sleep(20 * time.Millisecond)
	if got := messagePostCount(stub); got != 1 {
		t.Fatalf("message posts = %d, want 1 (status edited into answer)", got)
	}
	patches := stub.patchBodies()
	if len(patches) == 0 {
		t.Fatal("final delivery produced no edit")
	}
	var payload map[string]any
	_ = json.Unmarshal(patches[len(patches)-1], &payload)
	if content, _ := payload["content"].(string); !strings.Contains(content, "final answer") {
		t.Fatalf("final edit = %q, want answer replacing status", content)
	}
	if err := adapter.PresenceEnded(t.Context(), turnID); err != nil {
		t.Fatalf("PresenceEnded(): %v", err)
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
	adapter.statusAfter = 30 * time.Millisecond
	adapter.typingRefresh = 20 * time.Millisecond
	turnID := "turn-replace-1"
	req := presenceTestRequest(turnID)
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted(): %v", err)
	}
	if err := adapter.PresenceUpdated(t.Context(), req, "Looking through notes", "", false); err != nil {
		t.Fatalf("PresenceUpdated(): %v", err)
	}
	waitForStatusPost(t, stub)
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
	if err := adapter.PresenceEnded(t.Context(), turnID); err != nil {
		t.Fatalf("PresenceEnded(): %v", err)
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

func TestPresenceScreensSecretStatusAndArgs(t *testing.T) {
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
	req := presenceTestRequest("turn-secret-screened")
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted(): %v", err)
	}
	if err := adapter.PresenceUpdated(t.Context(), req, "Looking at secret-token abc123", `{"query":"secret hunter2"}`, false); err != nil {
		t.Fatalf("secret status update: %v", err)
	}
	for _, body := range outboundBodies(stub) {
		for _, leaked := range []string{"abc123", "hunter2", "secret-token"} {
			if strings.Contains(body, leaked) {
				t.Fatalf("outbound body leaked %q: %s", leaked, body)
			}
		}
	}
	if err := adapter.PresenceUpdated(t.Context(), req, "Looking through notes", `{"path":"notes.md","token":"sk-live-hunter2"}`, false); err != nil {
		t.Fatalf("clean status update: %v", err)
	}
	joined := strings.Join(outboundBodies(stub), "\n")
	if !strings.Contains(joined, "Looking through notes") {
		t.Errorf("outbound bodies = %q, want the clean activity text", joined)
	}
	if strings.Contains(joined, "sk-live-hunter2") {
		t.Errorf("outbound bodies = %q, leaked secret args", joined)
	}
	if err := adapter.PresenceEnded(t.Context(), "turn-secret-screened"); err != nil {
		t.Fatalf("PresenceEnded(): %v", err)
	}
}

func TestPresenceSingleSlowToolStaysAlive(t *testing.T) {
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
	adapter.statusAfter = 60 * time.Millisecond
	adapter.typingRefresh = 20 * time.Millisecond
	turnID := "turn-slow-tool-single"
	req := presenceTestRequest(turnID)
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted(): %v", err)
	}
	time.Sleep(25 * time.Millisecond)
	if err := adapter.PresenceUpdated(t.Context(), req, "Looking through notes", "", false); err != nil {
		t.Fatalf("single tool update: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool { return messagePostCount(stub) == 1 }, "timed status post")
	if got := typingCallCount(stub); got < 2 {
		t.Fatalf("typing calls = %d, want refresh before the status post", got)
	}
	time.Sleep(40 * time.Millisecond)
	if got := messagePostCount(stub); got != 1 {
		t.Fatalf("message posts = %d, want exactly one status message", got)
	}
	if err := adapter.PresenceUpdated(t.Context(), req, "Checking open tasks", "", false); err != nil {
		t.Fatalf("later activity update: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool { return len(stub.patchBodies()) >= 1 }, "status edit")
	patches := stub.patchBodies()
	var payload map[string]any
	_ = json.Unmarshal(patches[len(patches)-1], &payload)
	if content, _ := payload["content"].(string); !strings.Contains(content, "Checking open tasks") {
		t.Fatalf("edited status = %q, want later activity", content)
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
	patches = stub.patchBodies()
	_ = json.Unmarshal(patches[len(patches)-1], &payload)
	if content, _ := payload["content"].(string); !strings.Contains(content, "final answer") {
		t.Fatalf("final edit = %q, want answer replacing status", content)
	}
	if err := adapter.PresenceEnded(t.Context(), turnID); err != nil {
		t.Fatalf("PresenceEnded(): %v", err)
	}
}

func TestPresenceFailedStatusPostDoesNotBreakDelivery(t *testing.T) {
	var messagePosts atomic.Int32
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/typing") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/messages") {
			if messagePosts.Add(1) == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			writeMessage(t, w, "answer-id")
		}
	})
	adapter, _ := newSendTestAdapter(t, stub, func(cfg *config.Discord) {
		cfg.MinEditInterval = 0
	})
	adapter.statusAfter = time.Nanosecond
	adapter.typingRefresh = time.Hour
	turnID := "turn-status-post-failed"
	req := presenceTestRequest(turnID)
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted(): %v", err)
	}
	waitFor(t, 2*time.Second, func() bool { return messagePostCount(stub) == 1 }, "failed status post")
	deliverReq := &runtimechannelhost.DeliveryRequest{
		EffectID:       turnID,
		Channel:        "discord:owner",
		ConversationID: "channel:333",
		ReplyContext:   json.RawMessage(`{"channel_id":"333","message_id":"999"}`),
		Parts:          []runtimechannelhost.OutputPart{{Text: "final answer"}},
		IdempotencyKey: turnID,
	}
	receipt, err := adapter.Deliver(t.Context(), deliverReq)
	if err != nil {
		t.Fatalf("Deliver() after failed status post: %v", err)
	}
	if receipt.ExternalID != "answer-id" {
		t.Fatalf("receipt = %+v, want the answer message", receipt)
	}
	if patches := stub.patchBodies(); len(patches) != 0 {
		t.Fatalf("patches = %d, want none after an untracked status failure", len(patches))
	}
	joined := strings.Join(outboundBodies(stub), "\n")
	if !strings.Contains(joined, "final answer") {
		t.Fatalf("outbound bodies = %q, want the delivered answer", joined)
	}
	if err := adapter.PresenceEnded(t.Context(), turnID); err != nil {
		t.Fatalf("PresenceEnded(): %v", err)
	}
}
