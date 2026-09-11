package discord

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	runtimechannelhost "github.com/anggasct/aura/internal/runtime/channelhost"
)

type restCall struct {
	method      string
	path        string
	contentType string
	body        []byte
	auth        string
}

type restStub struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	calls   []restCall
	respond func(call int, w http.ResponseWriter, r *http.Request)
}

func newRestStub(t *testing.T, respond func(call int, w http.ResponseWriter, r *http.Request)) *restStub {
	t.Helper()
	stub := &restStub{t: t, respond: respond}
	stub.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		_ = r.Body.Close()
		stub.mu.Lock()
		stub.calls = append(stub.calls, restCall{
			method:      r.Method,
			path:        r.URL.Path,
			contentType: r.Header.Get("Content-Type"),
			body:        body,
			auth:        r.Header.Get("Authorization"),
		})
		call := len(stub.calls)
		stub.mu.Unlock()
		stub.respond(call, w, r)
	}))
	t.Cleanup(stub.srv.Close)
	return stub
}

func writeMessage(t *testing.T, w http.ResponseWriter, id string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"` + id + `","channel_id":"333"}`))
}

func (s *restStub) postCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, call := range s.calls {
		if call.method == http.MethodPost {
			n++
		}
	}
	return n
}

func (s *restStub) patchBodies() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	var bodies [][]byte
	for _, call := range s.calls {
		if call.method == http.MethodPatch {
			bodies = append(bodies, call.body)
		}
	}
	return bodies
}

func newSendTestAdapter(t *testing.T, stub *restStub, configure func(*config.Discord)) (*Adapter, *testDoubles) {
	t.Helper()
	t.Setenv("AURA_DISCORD_BOT_TOKEN", testToken)
	cfg := testAdapterConfig()
	if configure != nil {
		configure(&cfg)
	}
	doubles := &testDoubles{effects: newFakeEffectRunner(), media: &fakeMediaStore{}, sessions: &fakeSessionEnsurer{}}
	adapter, err := New(&cfg, &memoryResumeStore{}, doubles.effects, doubles.media, doubles.sessions, nil)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	adapter.rateCap = 30 * time.Millisecond
	adapter.restBase = stub.srv.URL
	return adapter, doubles
}

func deliverTestRequest(effectID, channel, text string) *runtimechannelhost.DeliveryRequest {
	return &runtimechannelhost.DeliveryRequest{
		EffectID:       effectID,
		Channel:        "discord",
		ConversationID: "dm:111",
		ReplyContext:   json.RawMessage(`{"channel_id":"333","message_id":"999"}`),
		Parts:          []runtimechannelhost.OutputPart{{Text: text}},
		IdempotencyKey: "key-" + effectID,
	}
}

func TestDeliver_PostsMessageWithReplyAndMentions(t *testing.T) {
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		writeMessage(t, w, "m1")
	})
	adapter, _ := newSendTestAdapter(t, stub, nil)

	receipt, err := adapter.Deliver(t.Context(), deliverTestRequest("e1", "discord", "hello @everyone"))
	if err != nil {
		t.Fatalf("Deliver(): %v", err)
	}
	if receipt.ExternalID != "m1" || receipt.ProviderID != "discord" {
		t.Errorf("receipt = %+v", receipt)
	}
	if stub.postCount() != 1 {
		t.Fatalf("posts = %d, want 1", stub.postCount())
	}
	stub.mu.Lock()
	call := stub.calls[0]
	stub.mu.Unlock()
	if call.auth != "Bot "+testToken {
		t.Error("send missing bot authorization")
	}
	var payload map[string]any
	if err := json.Unmarshal(call.body, &payload); err != nil {
		t.Fatalf("send body: %v", err)
	}
	content, _ := payload["content"].(string)
	if !strings.Contains(content, "@\u200beveryone") || strings.Contains(content, "@everyone ") && !strings.Contains(content, "@\u200beveryone") {
		t.Errorf("mentions not neutralized: %q", content)
	}
	mentions, _ := payload["allowed_mentions"].(map[string]any)
	if parse, _ := mentions["parse"].([]any); len(parse) != 0 {
		t.Errorf("allowed_mentions.parse = %v, want empty", parse)
	}
	reference, _ := payload["message_reference"].(map[string]any)
	if reference["message_id"] != "999" {
		t.Errorf("reply reference = %v", reference)
	}
}

func TestDeliver_SplitsLongText(t *testing.T) {
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		writeMessage(t, w, "m")
	})
	adapter, _ := newSendTestAdapter(t, stub, nil)

	text := strings.Repeat("a", 2000) + "\n" + strings.Repeat("b", 2000) + "\n" + strings.Repeat("c", 500)
	receipt, err := adapter.Deliver(t.Context(), deliverTestRequest("e2", "discord", text))
	if err != nil {
		t.Fatalf("Deliver(): %v", err)
	}
	if receipt.ExternalID == "" {
		t.Error("empty receipt")
	}
	if stub.postCount() != 3 {
		t.Fatalf("posts = %d, want 3", stub.postCount())
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.calls[0].body) > 2100 || len(stub.calls[1].body) > 2100 {
		t.Error("chunk exceeds the message bound")
	}
	var first, second map[string]any
	_ = json.Unmarshal(stub.calls[0].body, &first)
	_ = json.Unmarshal(stub.calls[1].body, &second)
	if _, ok := first["message_reference"]; !ok {
		t.Error("first chunk carries no reply reference")
	}
	if _, ok := second["message_reference"]; ok {
		t.Error("follow-up chunk carries a reply reference")
	}
	joined := strings.Builder{}
	for _, call := range stub.calls {
		var payload map[string]any
		_ = json.Unmarshal(call.body, &payload)
		content, _ := payload["content"].(string)
		joined.WriteString(content)
	}
	if joined.String() != text {
		t.Error("split chunks do not reassemble to the original text")
	}
}

func TestSplitMessage_RuneSafe(t *testing.T) {
	text := strings.Repeat("🎉", 1500)
	chunks := splitMessage(text)
	for _, chunk := range chunks {
		if len([]rune(chunk)) > discordMessageLimit {
			t.Fatalf("chunk of %d runes exceeds the limit", len([]rune(chunk)))
		}
	}
	if strings.Join(chunks, "") != text {
		t.Error("chunks do not reassemble")
	}
}

func TestSplitMessage_LongNonASCIIRun(t *testing.T) {
	text := strings.Repeat("中", 2500)
	chunks := splitMessage(text)
	if len(chunks) != 2 {
		t.Fatalf("chunks = %d, want 2", len(chunks))
	}
	for _, chunk := range chunks {
		if got := len([]rune(chunk)); got > discordMessageLimit {
			t.Fatalf("chunk of %d runes exceeds the limit", got)
		}
	}
	if got := len([]rune(chunks[0])); got != discordMessageLimit {
		t.Errorf("first chunk = %d runes, want %d", got, discordMessageLimit)
	}
	if strings.Join(chunks, "") != text {
		t.Error("chunks do not reassemble")
	}
}

func TestDeliver_UploadsRemainderAsFile(t *testing.T) {
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		writeMessage(t, w, "m")
	})
	adapter, _ := newSendTestAdapter(t, stub, nil)

	text := strings.Repeat("x", 2000*3+100)
	if _, err := adapter.Deliver(t.Context(), deliverTestRequest("e3", "discord", text)); err != nil {
		t.Fatalf("Deliver(): %v", err)
	}
	if stub.postCount() != 4 {
		t.Fatalf("posts = %d, want 3 messages plus 1 upload", stub.postCount())
	}
	stub.mu.Lock()
	upload := stub.calls[3]
	stub.mu.Unlock()
	if !strings.HasPrefix(upload.contentType, "multipart/form-data") {
		t.Fatalf("upload content type = %q", upload.contentType)
	}
	if !strings.Contains(string(upload.body), "message.txt") || !strings.Contains(string(upload.body), "payload_json") {
		t.Error("upload misses the file or the payload part")
	}
}

func TestDeliver_RejectsOversizeTotal(t *testing.T) {
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		writeMessage(t, w, "m")
	})
	adapter, _ := newSendTestAdapter(t, stub, func(cfg *config.Discord) {
		cfg.MaxAttachmentBytes = 100
	})

	_, err := adapter.Deliver(t.Context(), deliverTestRequest("e4", "discord", strings.Repeat("y", 2000*3+200)))
	if code, ok := CodeOf(err); !ok || code != ErrorCodeMessageTooLarge {
		t.Fatalf("code = %v, %v; want message_too_large", code, ok)
	}
	if stub.postCount() != 0 {
		t.Errorf("posts = %d, want none", stub.postCount())
	}
}

func TestDeliver_RetriesRateLimitThenSucceeds(t *testing.T) {
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		if call < 3 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeMessage(t, w, "m1")
	})
	adapter, _ := newSendTestAdapter(t, stub, nil)

	if _, err := adapter.Deliver(t.Context(), deliverTestRequest("e5", "discord", "hi")); err != nil {
		t.Fatalf("Deliver(): %v", err)
	}
	if stub.postCount() != 3 {
		t.Errorf("posts = %d, want 3", stub.postCount())
	}
}

func TestDeliver_RateLimitExhaustionIsUnknown(t *testing.T) {
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	adapter, _ := newSendTestAdapter(t, stub, nil)

	_, err := adapter.Deliver(t.Context(), deliverTestRequest("e6", "discord", "hi"))
	if code, ok := CodeOf(err); !ok || code != ErrorCodeDeliveryAmbiguous {
		t.Fatalf("code = %v, %v; want delivery_ambiguous", code, ok)
	}
}

func TestDeliver_RejectedTokenFailsDelivery(t *testing.T) {
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	adapter, _ := newSendTestAdapter(t, stub, nil)

	_, err := adapter.Deliver(t.Context(), deliverTestRequest("e7", "discord", "hi"))
	if code, ok := CodeOf(err); !ok || code != ErrorCodeDeliveryFailed {
		t.Fatalf("code = %v, %v; want delivery_failed", code, ok)
	}
}

func TestDeliver_CrashWindowNeverReposts(t *testing.T) {
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		writeMessage(t, w, "m1")
	})
	adapter, doubles := newSendTestAdapter(t, stub, nil)
	doubles.effects.crashAfterInvoke = true

	_, err := adapter.Deliver(t.Context(), deliverTestRequest("e8", "discord", "hi"))
	if err == nil {
		t.Fatal("crashed delivery succeeded")
	}
	doubles.effects.crashAfterInvoke = false
	_, err = adapter.Deliver(t.Context(), deliverTestRequest("e8", "discord", "hi"))
	if code, ok := CodeOf(err); !ok || code != ErrorCodeDeliveryAmbiguous {
		t.Fatalf("code = %v, %v; want delivery_ambiguous", code, ok)
	}
	if stub.postCount() != 1 {
		t.Errorf("posts = %d, want exactly 1", stub.postCount())
	}
}

func TestDeliver_RestartReplaysWithoutRepost(t *testing.T) {
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		writeMessage(t, w, "m1")
	})
	adapter, doubles := newSendTestAdapter(t, stub, nil)

	first, err := adapter.Deliver(t.Context(), deliverTestRequest("e9", "discord", "hi"))
	if err != nil {
		t.Fatalf("first Deliver(): %v", err)
	}
	restartedCfg := testAdapterConfig()
	restarted, err := New(&restartedCfg, &memoryResumeStore{}, doubles.effects, doubles.media, doubles.sessions, nil)
	if err != nil {
		t.Fatalf("restart New(): %v", err)
	}
	restarted.restBase = stub.srv.URL
	second, err := restarted.Deliver(t.Context(), deliverTestRequest("e9", "discord", "hi"))
	if err != nil {
		t.Fatalf("second Deliver(): %v", err)
	}
	if first.ExternalID != second.ExternalID {
		t.Errorf("receipts differ: %+v vs %+v", first, second)
	}
	if stub.postCount() != 1 {
		t.Errorf("posts = %d, want 1", stub.postCount())
	}
}

func TestDeliver_CoalescesRapidEdits(t *testing.T) {
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		writeMessage(t, w, "m")
	})
	adapter, _ := newSendTestAdapter(t, stub, func(cfg *config.Discord) {
		cfg.MinEditInterval = 150000000
	})

	if _, err := adapter.Deliver(t.Context(), deliverTestRequest("e10", "discord", "version one")); err != nil {
		t.Fatalf("post: %v", err)
	}
	type editOutcome struct {
		err error
	}
	outcomes := make(chan editOutcome, 2)
	go func() {
		_, err := adapter.Deliver(t.Context(), deliverTestRequest("e10", "discord", "version two"))
		outcomes <- editOutcome{err: err}
	}()
	time.Sleep(20 * time.Millisecond)
	_, err := adapter.Deliver(t.Context(), deliverTestRequest("e10", "discord", "version three"))
	if err != nil {
		t.Fatalf("latest edit: %v", err)
	}
	outcome := <-outcomes
	if outcome.err != nil {
		t.Fatalf("superseded edit: %v", outcome.err)
	}
	if stub.postCount() != 1 {
		t.Errorf("posts = %d, want 1", stub.postCount())
	}
	patches := stub.patchBodies()
	if len(patches) != 1 {
		t.Fatalf("patches = %d, want 1", len(patches))
	}
	var payload map[string]any
	_ = json.Unmarshal(patches[0], &payload)
	if payload["content"] != "version three" {
		t.Errorf("patched content = %v, want version three", payload["content"])
	}
}

func TestDeliver_RejectsInvalid(t *testing.T) {
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		writeMessage(t, w, "m")
	})
	adapter, _ := newSendTestAdapter(t, stub, nil)

	if _, err := adapter.Deliver(t.Context(), nil); err == nil {
		t.Error("nil request accepted")
	}
	empty := deliverTestRequest("e11", "discord", "   ")
	if _, err := adapter.Deliver(t.Context(), empty); err == nil {
		t.Error("empty text accepted")
	}
	badChannel := deliverTestRequest("e12", "discord", "hi")
	badChannel.Channel = "discord"
	badChannel.ReplyContext = nil
	if _, err := adapter.Deliver(t.Context(), badChannel); err == nil {
		t.Error("non-id channel accepted")
	}
	badReply := deliverTestRequest("e13", "discord", "hi")
	badReply.ReplyContext = json.RawMessage(`{oops`)
	if _, err := adapter.Deliver(t.Context(), badReply); err == nil {
		t.Error("broken reply context accepted")
	}
	if stub.postCount() != 0 {
		t.Errorf("posts = %d, want none", stub.postCount())
	}
}
