package discord

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/anggasct/aura/internal/channels/kit/status"
	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/secret"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return fn(r)
}

type timedRestCall struct {
	method string
	path   string
	body   []byte
	at     time.Time
}

type memoryDiscordAPI struct {
	mu    sync.Mutex
	calls []timedRestCall
}

func newMemoryDiscordAdapter(t *testing.T, configure func(*Config)) (*Adapter, *memoryDiscordAPI) {
	t.Helper()
	t.Setenv("AURA_DISCORD_BOT_TOKEN", testToken)
	cfg := testAdapterConfig()
	if configure != nil {
		configure(&cfg)
	}
	api := &memoryDiscordAPI{}
	doubles := &testDoubles{effects: newFakeEffectRunner(), media: &fakeMediaStore{}, sessions: &fakeSessionEnsurer{}}
	adapter, err := newRoutedTestAdapter(t, &cfg, &memoryResumeStore{}, doubles.effects, doubles.media, doubles.sessions, nil)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	adapter.restBase = "https://discord.test/api/v10"
	adapter.httpClient = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			var body []byte
			if r.Body != nil {
				body, _ = io.ReadAll(r.Body)
				_ = r.Body.Close()
			}
			api.mu.Lock()
			api.calls = append(api.calls, timedRestCall{
				method: r.Method,
				path:   r.URL.Path,
				body:   body,
				at:     time.Now(),
			})
			api.mu.Unlock()
			rec := httptest.NewRecorder()
			if strings.HasSuffix(r.URL.Path, "/typing") {
				rec.WriteHeader(http.StatusNoContent)
			} else {
				rec.Header().Set("Content-Type", "application/json")
				_, _ = rec.WriteString(`{"id":"status-id","channel_id":"333"}`)
			}
			resp := rec.Result()
			resp.Request = r
			return resp, nil
		}),
	}
	return adapter, api
}

func (m *memoryDiscordAPI) typingCalls() []timedRestCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []timedRestCall
	for _, call := range m.calls {
		if call.method == http.MethodPost && strings.HasSuffix(call.path, "/typing") {
			out = append(out, call)
		}
	}
	return out
}

func (m *memoryDiscordAPI) messagePosts() []timedRestCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []timedRestCall
	for _, call := range m.calls {
		if call.method == http.MethodPost && strings.HasSuffix(call.path, "/messages") {
			out = append(out, call)
		}
	}
	return out
}

func (m *memoryDiscordAPI) patchContents(t *testing.T) []string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, call := range m.calls {
		if call.method != http.MethodPatch {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(call.body, &payload); err != nil {
			t.Fatalf("patch body: %v", err)
		}
		content, _ := payload["content"].(string)
		out = append(out, content)
	}
	return out
}

func presenceTestRequest(turnID string) *runtimechannelhost.DeliveryRequest {
	return &runtimechannelhost.DeliveryRequest{
		EffectID:       turnID,
		Channel:        "discord:owner",
		ConversationID: "channel:333",
		ReplyContext:   json.RawMessage(`{"channel_id":"333","message_id":"999"}`),
		IdempotencyKey: turnID,
	}
}

func stepUpdate(callID, activity string, outcome runtimechannelhost.StepOutcome) runtimechannelhost.PresenceUpdate {
	return runtimechannelhost.PresenceUpdate{
		CallIDs:  []string{callID},
		Activity: activity,
		Outcome:  outcome,
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

func TestTypingContinuesAfterTheStatusIsPosted(t *testing.T) {
	t.Run("stops on Deliver", func(t *testing.T) {
		adapter, api := newMemoryDiscordAdapter(t, func(cfg *Config) {
			cfg.MinEditInterval = configkit.Duration(2 * time.Second)
		})
		turnID := "turn-typing-deliver"
		req := presenceTestRequest(turnID)
		deliverReq := &runtimechannelhost.DeliveryRequest{
			EffectID:       turnID,
			Channel:        "discord:owner",
			ConversationID: "channel:333",
			ReplyContext:   json.RawMessage(`{"channel_id":"333","message_id":"999"}`),
			Parts:          []runtimechannelhost.OutputPart{{Text: "final answer"}},
			IdempotencyKey: turnID,
		}

		synctest.Test(t, func(t *testing.T) {
			if err := adapter.PresenceStarted(t.Context(), req); err != nil {
				t.Fatalf("PresenceStarted(): %v", err)
			}
			synctest.Wait()
			if got := len(api.typingCalls()); got != 1 {
				t.Fatalf("initial typing calls = %d, want 1", got)
			}

			time.Sleep(statusPostAfter)
			synctest.Wait()
			posts := api.messagePosts()
			if len(posts) != 1 {
				t.Fatalf("status posts = %d, want 1", len(posts))
			}

			time.Sleep(24 * time.Second)
			synctest.Wait()
			afterPost := 0
			for _, call := range api.typingCalls() {
				if call.at.After(posts[0].at) {
					afterPost++
				}
			}
			if afterPost < 3 {
				t.Fatalf("typing calls after status post = %d, want at least 3", afterPost)
			}

			if _, err := adapter.Deliver(t.Context(), deliverReq); err != nil {
				t.Fatalf("Deliver(): %v", err)
			}
			typingAtDeliver := len(api.typingCalls())
			time.Sleep(30 * time.Second)
			synctest.Wait()
			if got := len(api.typingCalls()); got != typingAtDeliver {
				t.Fatalf("typing calls after Deliver = %d, want still %d", got, typingAtDeliver)
			}
			if err := adapter.PresenceEnded(t.Context(), turnID, runtimechannelhost.PresenceEnd{}); err != nil {
				t.Fatalf("PresenceEnded(): %v", err)
			}
		})
	})

	t.Run("stops on PresenceEnded", func(t *testing.T) {
		adapter, api := newMemoryDiscordAdapter(t, func(cfg *Config) {
			cfg.MinEditInterval = configkit.Duration(2 * time.Second)
		})
		turnID := "turn-typing-end"
		req := presenceTestRequest(turnID)

		synctest.Test(t, func(t *testing.T) {
			if err := adapter.PresenceStarted(t.Context(), req); err != nil {
				t.Fatalf("PresenceStarted(): %v", err)
			}
			time.Sleep(statusPostAfter + 24*time.Second)
			synctest.Wait()
			posts := api.messagePosts()
			if len(posts) != 1 {
				t.Fatalf("status posts = %d, want 1", len(posts))
			}
			afterPost := 0
			for _, call := range api.typingCalls() {
				if call.at.After(posts[0].at) {
					afterPost++
				}
			}
			if afterPost < 3 {
				t.Fatalf("typing calls after status post = %d, want at least 3", afterPost)
			}
			if err := adapter.PresenceEnded(t.Context(), turnID, runtimechannelhost.PresenceEnd{}); err != nil {
				t.Fatalf("PresenceEnded(): %v", err)
			}
			typingAtEnd := len(api.typingCalls())
			time.Sleep(30 * time.Second)
			synctest.Wait()
			if got := len(api.typingCalls()); got != typingAtEnd {
				t.Fatalf("typing calls after PresenceEnded = %d, want still %d", got, typingAtEnd)
			}
		})
	})
}

func TestElapsedTimeTicksWithoutActivity(t *testing.T) {
	t.Run("stops on Deliver", func(t *testing.T) {
		adapter, api := newMemoryDiscordAdapter(t, func(cfg *Config) {
			cfg.MinEditInterval = configkit.Duration(2 * time.Second)
		})
		turnID := "turn-tick-deliver"
		req := presenceTestRequest(turnID)
		deliverReq := &runtimechannelhost.DeliveryRequest{
			EffectID:       turnID,
			Channel:        "discord:owner",
			ConversationID: "channel:333",
			ReplyContext:   json.RawMessage(`{"channel_id":"333","message_id":"999"}`),
			Parts:          []runtimechannelhost.OutputPart{{Text: "final answer"}},
			IdempotencyKey: turnID,
		}

		synctest.Test(t, func(t *testing.T) {
			if err := adapter.PresenceStarted(t.Context(), req); err != nil {
				t.Fatalf("PresenceStarted(): %v", err)
			}
			time.Sleep(statusPostAfter)
			synctest.Wait()
			posts := api.messagePosts()
			if len(posts) != 1 {
				t.Fatalf("status posts = %d, want 1", len(posts))
			}
			var posted map[string]any
			if err := json.Unmarshal(posts[0].body, &posted); err != nil {
				t.Fatalf("post body: %v", err)
			}
			if posted["content"] != "Working…\n⏱ 0:10" {
				t.Fatalf("initial status = %q, want Working… at 0:10", posted["content"])
			}

			time.Sleep(90 * time.Second)
			synctest.Wait()
			got := api.patchContents(t)
			want := []string{
				"Working…\n⏱ 0:40",
				"Still working…\n⏱ 1:10",
				"Still working…\n⏱ 1:40",
			}
			if len(got) != len(want) {
				t.Fatalf("idle edits = %d (%q), want %d", len(got), got, len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("edit[%d] = %q, want %q", i, got[i], want[i])
				}
			}

			if _, err := adapter.Deliver(t.Context(), deliverReq); err != nil {
				t.Fatalf("Deliver(): %v", err)
			}
			patchesAfterDeliver := len(api.patchContents(t))
			time.Sleep(90 * time.Second)
			synctest.Wait()
			if got := len(api.patchContents(t)); got != patchesAfterDeliver {
				t.Fatalf("edits after Deliver = %d, want still %d", got, patchesAfterDeliver)
			}
			if err := adapter.PresenceEnded(t.Context(), turnID, runtimechannelhost.PresenceEnd{}); err != nil {
				t.Fatalf("PresenceEnded(): %v", err)
			}
		})
	})

	t.Run("stops on PresenceEnded", func(t *testing.T) {
		adapter, api := newMemoryDiscordAdapter(t, func(cfg *Config) {
			cfg.MinEditInterval = configkit.Duration(2 * time.Second)
		})
		turnID := "turn-tick-end"
		req := presenceTestRequest(turnID)

		synctest.Test(t, func(t *testing.T) {
			if err := adapter.PresenceStarted(t.Context(), req); err != nil {
				t.Fatalf("PresenceStarted(): %v", err)
			}
			time.Sleep(statusPostAfter + 60*time.Second)
			synctest.Wait()
			if got := len(api.patchContents(t)); got != 2 {
				t.Fatalf("idle edits = %d, want 2", got)
			}
			if err := adapter.PresenceEnded(t.Context(), turnID, runtimechannelhost.PresenceEnd{}); err != nil {
				t.Fatalf("PresenceEnded(): %v", err)
			}
			time.Sleep(90 * time.Second)
			synctest.Wait()
			if got := len(api.patchContents(t)); got != 2 {
				t.Fatalf("edits after PresenceEnded = %d, want still 2", got)
			}
		})
	})
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
	if err := adapter.PresenceEnded(t.Context(), "turn-fast-1", runtimechannelhost.PresenceEnd{}); err != nil {
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
	adapter, _ := newSendTestAdapter(t, stub, func(cfg *Config) {
		cfg.MinEditInterval = 0
	})
	adapter.statusAfter = 50 * time.Millisecond
	adapter.typingRefresh = 20 * time.Millisecond
	req := presenceTestRequest("turn-slow-1")
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted(): %v", err)
	}
	if err := adapter.PresenceUpdated(t.Context(), req, stepUpdate("c1", "Looking through notes", runtimechannelhost.StepRunning)); err != nil {
		t.Fatalf("first PresenceUpdated(): %v", err)
	}
	waitForStatusPost(t, stub)
	if err := adapter.PresenceUpdated(t.Context(), req, stepUpdate("c2", "Checking open tasks", runtimechannelhost.StepRunning)); err != nil {
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
	if !strings.Contains(content, "⏳ Looking through notes") || !strings.Contains(content, "⏳ Checking open tasks") {
		t.Errorf("edited content = %q, want both steps", content)
	}
	if err := adapter.PresenceEnded(t.Context(), "turn-slow-1", runtimechannelhost.PresenceEnd{}); err != nil {
		t.Fatalf("PresenceEnded(): %v", err)
	}
}

func TestPresenceSilentFallback(t *testing.T) {
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		writeMessage(t, w, "status-id")
	})
	adapter, _ := newSendTestAdapter(t, stub, func(cfg *Config) {
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
	if !strings.Contains(body, status.DefaultTexts().Working) || !strings.Contains(body, status.MarkerElapsed) {
		t.Errorf("fallback body = %q, want working text and elapsed time", body)
	}
	if err := adapter.PresenceEnded(t.Context(), "turn-silent-1", runtimechannelhost.PresenceEnd{}); err != nil {
		t.Fatalf("PresenceEnded(): %v", err)
	}
}

func TestPresenceRedactsSecrets(t *testing.T) {
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		writeMessage(t, w, "status-id")
	})
	adapter, _ := newSendTestAdapter(t, stub, func(cfg *Config) {
		cfg.MinEditInterval = 0
	})
	adapter.statusAfter = 30 * time.Millisecond
	vault := secret.New()
	vault.Set("probe", "hunter2-secret-value")
	adapter.secretVault = vault
	req := presenceTestRequest("turn-secret-1")
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted(): %v", err)
	}
	if err := adapter.PresenceUpdated(t.Context(), req, stepUpdate("c1", "Checking hunter2-secret-value", runtimechannelhost.StepRunning)); err != nil {
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
	if err := adapter.PresenceEnded(t.Context(), "turn-secret-1", runtimechannelhost.PresenceEnd{}); err != nil {
		t.Fatalf("PresenceEnded(): %v", err)
	}
}

func TestPresenceStepFailureMarksOnlyThatStep(t *testing.T) {
	adapter, api := newMemoryDiscordAdapter(t, func(cfg *Config) {
		cfg.MinEditInterval = 0
	})
	adapter.statusAfter = time.Nanosecond
	req := presenceTestRequest("turn-fail-step")
	synctest.Test(t, func(t *testing.T) {
		if err := adapter.PresenceStarted(t.Context(), req); err != nil {
			t.Fatalf("PresenceStarted(): %v", err)
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if err := adapter.PresenceUpdated(t.Context(), req, stepUpdate("c1", "Mencari catatan", runtimechannelhost.StepRunning)); err != nil {
			t.Fatalf("activity update: %v", err)
		}
		if err := adapter.PresenceUpdated(t.Context(), req, stepUpdate("c1", "Mencari catatan", runtimechannelhost.StepFailed)); err != nil {
			t.Fatalf("failure update: %v", err)
		}
		if err := adapter.PresenceUpdated(t.Context(), req, stepUpdate("c2", "Membaca ringkasan", runtimechannelhost.StepRunning)); err != nil {
			t.Fatalf("second activity update: %v", err)
		}
		patches := api.patchContents(t)
		if len(patches) == 0 {
			t.Fatal("step failure produced no edit")
		}
		last := patches[len(patches)-1]
		if !strings.HasPrefix(last, "✗ Mencari catatan\n⏳ Membaca ringkasan\n⏱ ") {
			t.Errorf("last status = %q, want failed first step and running second step", last)
		}
		if strings.Contains(last, "⚠️") {
			t.Errorf("last status contains legacy warning marker: %q", last)
		}
		if err := adapter.PresenceEnded(t.Context(), "turn-fail-step", runtimechannelhost.PresenceEnd{}); err != nil {
			t.Fatalf("PresenceEnded(): %v", err)
		}
	})
}

func TestPresenceThrottleCoalesces(t *testing.T) {
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		writeMessage(t, w, "status-id")
	})
	adapter, _ := newSendTestAdapter(t, stub, func(cfg *Config) {
		cfg.MinEditInterval = 3600000000000
	})
	adapter.statusAfter = 30 * time.Millisecond
	req := presenceTestRequest("turn-throttle-1")
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted(): %v", err)
	}
	if err := adapter.PresenceUpdated(t.Context(), req, stepUpdate("c1", "First activity here", runtimechannelhost.StepRunning)); err != nil {
		t.Fatalf("first update: %v", err)
	}
	waitForStatusPost(t, stub)
	if err := adapter.PresenceUpdated(t.Context(), req, stepUpdate("c2", "Second activity here", runtimechannelhost.StepRunning)); err != nil {
		t.Fatalf("coalesced update returned error")
	}
	if patches := stub.patchBodies(); len(patches) != 0 {
		t.Fatalf("patches = %d, want 0 while throttled", len(patches))
	}
	if err := adapter.PresenceEnded(t.Context(), "turn-throttle-1", runtimechannelhost.PresenceEnd{}); err != nil {
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
	adapter, _ := newSendTestAdapter(t, stub, func(cfg *Config) {
		cfg.MinEditInterval = configkit.Duration(time.Hour)
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
		updateDone <- adapter.PresenceUpdated(t.Context(), req, stepUpdate("c2", "Second activity here", runtimechannelhost.StepRunning))
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
	if err := adapter.PresenceEnded(t.Context(), turnID, runtimechannelhost.PresenceEnd{}); err != nil {
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
	adapter, _ := newSendTestAdapter(t, stub, func(cfg *Config) {
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
	if got := messagePostCount(stub); got != 2 {
		t.Fatalf("message posts = %d, want 2 (status and final answer)", got)
	}
	if patches := stub.patchBodies(); len(patches) != 0 {
		t.Fatalf("patches = %d, want 0 (status is deleted, not edited)", len(patches))
	}
	sends := stub.messageSends(0)
	var payload map[string]any
	_ = json.Unmarshal(sends[len(sends)-1].body, &payload)
	if content, _ := payload["content"].(string); !strings.Contains(content, "final answer") {
		t.Fatalf("final post = %q, want final answer", content)
	}
	if err := adapter.PresenceEnded(t.Context(), turnID, runtimechannelhost.PresenceEnd{}); err != nil {
		t.Fatalf("PresenceEnded(): %v", err)
	}
}

func TestPresenceStatusReplacedByFinalDelivery(t *testing.T) {
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/typing") || r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeMessage(t, w, "status-id")
	})
	adapter, _ := newSendTestAdapter(t, stub, func(cfg *Config) {
		cfg.MinEditInterval = 0
	})
	adapter.statusAfter = 30 * time.Millisecond
	adapter.typingRefresh = 20 * time.Millisecond
	turnID := "turn-replace-1"
	req := presenceTestRequest(turnID)
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted(): %v", err)
	}
	if err := adapter.PresenceUpdated(t.Context(), req, stepUpdate("c1", "Looking through notes", runtimechannelhost.StepRunning)); err != nil {
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
	if messagePostCount(stub) != 2 {
		t.Fatalf("message posts = %d, want 2 (status and final answer)", messagePostCount(stub))
	}
	if patches := stub.patchBodies(); len(patches) != 0 {
		t.Fatalf("patches = %d, want 0", len(patches))
	}
	sends := stub.messageSends(0)
	var payload map[string]any
	_ = json.Unmarshal(sends[len(sends)-1].body, &payload)
	content, _ := payload["content"].(string)
	if !strings.Contains(content, "final answer") {
		t.Errorf("final content = %q, want final answer", content)
	}
	if err := adapter.PresenceEnded(t.Context(), turnID, runtimechannelhost.PresenceEnd{}); err != nil {
		t.Fatalf("PresenceEnded(): %v", err)
	}
}

func TestPresenceScreensSecretStatus(t *testing.T) {
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/typing") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeMessage(t, w, "status-id")
	})
	adapter, _ := newSendTestAdapter(t, stub, func(cfg *Config) {
		cfg.MinEditInterval = 0
	})
	adapter.statusAfter = time.Nanosecond
	req := presenceTestRequest("turn-secret-screened")
	if err := adapter.PresenceStarted(t.Context(), req); err != nil {
		t.Fatalf("PresenceStarted(): %v", err)
	}
	if err := adapter.PresenceUpdated(t.Context(), req, stepUpdate("c1", "Looking at secret-token abc123", runtimechannelhost.StepRunning)); err != nil {
		t.Fatalf("secret status update: %v", err)
	}
	for _, body := range outboundBodies(stub) {
		for _, leaked := range []string{"abc123", "secret-token"} {
			if strings.Contains(body, leaked) {
				t.Fatalf("outbound body leaked %q: %s", leaked, body)
			}
		}
	}
	if err := adapter.PresenceUpdated(t.Context(), req, stepUpdate("c2", "Looking through notes", runtimechannelhost.StepRunning)); err != nil {
		t.Fatalf("clean status update: %v", err)
	}
	joined := strings.Join(outboundBodies(stub), "\n")
	if !strings.Contains(joined, "Looking through notes") {
		t.Errorf("outbound bodies = %q, want the clean activity text", joined)
	}
	if err := adapter.PresenceEnded(t.Context(), "turn-secret-screened", runtimechannelhost.PresenceEnd{}); err != nil {
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
	adapter, _ := newSendTestAdapter(t, stub, func(cfg *Config) {
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
	if err := adapter.PresenceUpdated(t.Context(), req, stepUpdate("c1", "Looking through notes", runtimechannelhost.StepRunning)); err != nil {
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
	if err := adapter.PresenceUpdated(t.Context(), req, stepUpdate("c2", "Checking open tasks", runtimechannelhost.StepRunning)); err != nil {
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
	sends := stub.messageSends(0)
	if len(sends) != 2 {
		t.Fatalf("posts = %d, want status and final answer", len(sends))
	}
	_ = json.Unmarshal(sends[len(sends)-1].body, &payload)
	if content, _ := payload["content"].(string); !strings.Contains(content, "final answer") {
		t.Fatalf("final post = %q, want final answer", content)
	}
	if err := adapter.PresenceEnded(t.Context(), turnID, runtimechannelhost.PresenceEnd{}); err != nil {
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
	adapter, _ := newSendTestAdapter(t, stub, func(cfg *Config) {
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
	if err := adapter.PresenceEnded(t.Context(), turnID, runtimechannelhost.PresenceEnd{}); err != nil {
		t.Fatalf("PresenceEnded(): %v", err)
	}
}

func TestLateInitialStatusNeverOverwritesANewerStatus(t *testing.T) {
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		writeMessage(t, w, "status-id")
	})
	adapter, _ := newSendTestAdapter(t, stub, func(cfg *Config) {
		cfg.MinEditInterval = 0
	})
	session, _, err := adapter.ensurePresence(presenceTestRequest("turn-late-initial"))
	if err != nil {
		t.Fatalf("ensurePresence(): %v", err)
	}
	adapter.presenceMu.Lock()
	session.steps = status.ApplyUpdate(session.steps, stepUpdate("c1", "Checking tasks", runtimechannelhost.StepRunning))
	adapter.presenceMu.Unlock()
	session.statusMu.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		adapter.postInitialStatus(t.Context(), session)
	}()
	time.Sleep(50 * time.Millisecond)
	firstDisplay, firstVersion := adapter.renderSession(session, time.Now())
	if err := adapter.postOrPatchStatusLocked(t.Context(), session, firstDisplay, firstVersion); err != nil {
		t.Fatalf("status post: %v", err)
	}
	adapter.presenceMu.Lock()
	session.steps = status.ApplyUpdate(session.steps, stepUpdate("c1", "Checking tasks", runtimechannelhost.StepFailed))
	session.version++
	adapter.presenceMu.Unlock()
	failedDisplay, failedVersion := adapter.renderSession(session, time.Now())
	if err := adapter.postOrPatchStatusLocked(t.Context(), session, failedDisplay, failedVersion); err != nil {
		t.Fatalf("failure edit: %v", err)
	}
	session.statusMu.Unlock()
	<-done
	if posts := messagePostCount(stub); posts != 1 {
		t.Fatalf("status posts = %d, want 1", posts)
	}
	patches := stub.patchBodies()
	if len(patches) != 1 {
		t.Fatalf("patches = %d, want only the failure edit", len(patches))
	}
	var payload map[string]any
	_ = json.Unmarshal(patches[0], &payload)
	if content, _ := payload["content"].(string); content != failedDisplay {
		t.Fatalf("final status = %q, want %q", content, failedDisplay)
	}
}

func TestPresenceUpdateDuringInFlightPostFlushesAfterMinEditInterval(t *testing.T) {
	adapter, api := newMemoryDiscordAdapter(t, func(cfg *Config) {
		cfg.MinEditInterval = configkit.Duration(2 * time.Second)
	})
	baseTransport := adapter.httpClient.Transport
	req := presenceTestRequest("turn-inflight-flush")

	synctest.Test(t, func(t *testing.T) {
		postStarted := make(chan struct{})
		releasePost := make(chan struct{})
		var postCalls atomic.Int32
		adapter.httpClient.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/messages") && postCalls.Add(1) == 1 {
				close(postStarted)
				<-releasePost
			}
			return baseTransport.RoundTrip(r)
		})

		if err := adapter.PresenceStarted(t.Context(), req); err != nil {
			t.Fatalf("PresenceStarted(): %v", err)
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
			t.Fatalf("PresenceUpdated(): %v", err)
		}
		synctest.Wait()
		if got := len(api.patchContents(t)); got != 0 {
			t.Fatalf("edits immediately after post = %d, want 0 within min_edit_interval", got)
		}

		time.Sleep(2 * time.Second)
		synctest.Wait()
		edits := api.patchContents(t)
		if len(edits) != 1 {
			t.Fatalf("edits after min_edit_interval = %d, want 1", len(edits))
		}
		if !strings.HasPrefix(edits[0], "⏳ Inspecting logs\n⏱ ") {
			t.Errorf("flushed edit = %q, want step queued during in-flight post", edits[0])
		}
		if err := adapter.PresenceEnded(t.Context(), "turn-inflight-flush", runtimechannelhost.PresenceEnd{}); err != nil {
			t.Fatalf("PresenceEnded(): %v", err)
		}
	})
}

func TestInitialStatusDefersDuringTypingRateHold(t *testing.T) {
	adapter, api := newMemoryDiscordAdapter(t, func(cfg *Config) {
		cfg.MinEditInterval = configkit.Duration(2 * time.Second)
	})
	baseTransport := adapter.httpClient.Transport
	var typingCalls atomic.Int32
	adapter.httpClient.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/typing") && typingCalls.Add(1) == 2 {
			rec := httptest.NewRecorder()
			rec.Header().Set("Retry-After", "5")
			rec.WriteHeader(http.StatusTooManyRequests)
			resp := rec.Result()
			resp.Request = r
			return resp, nil
		}
		return baseTransport.RoundTrip(r)
	})

	req := presenceTestRequest("turn-typing-hold")
	synctest.Test(t, func(t *testing.T) {
		if err := adapter.PresenceStarted(t.Context(), req); err != nil {
			t.Fatalf("PresenceStarted(): %v", err)
		}
		time.Sleep(typingRefreshInterval)
		synctest.Wait()

		time.Sleep(statusPostAfter - typingRefreshInterval)
		synctest.Wait()
		if got := len(api.messagePosts()); got != 0 {
			t.Fatalf("status posts during active typing rate hold = %d, want 0", got)
		}

		time.Sleep(2 * time.Second)
		synctest.Wait()
		if got := len(api.messagePosts()); got != 0 {
			t.Fatalf("status posts before rate hold expiry = %d, want 0", got)
		}

		time.Sleep(time.Second)
		synctest.Wait()
		posts := api.messagePosts()
		if len(posts) != 1 {
			t.Fatalf("status posts after rate hold expiry = %d, want 1", len(posts))
		}
		var payload map[string]any
		if err := json.Unmarshal(posts[0].body, &payload); err != nil {
			t.Fatalf("post body: %v", err)
		}
		if payload["content"] != "Working…\n⏱ 0:13" {
			t.Errorf("deferred initial status = %q, want Working… at 0:13", payload["content"])
		}
		if err := adapter.PresenceEnded(t.Context(), "turn-typing-hold", runtimechannelhost.PresenceEnd{}); err != nil {
			t.Fatalf("PresenceEnded(): %v", err)
		}
	})
}
