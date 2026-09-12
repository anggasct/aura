package discord

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type cardPost struct {
	channel string
	body    map[string]any
}

type callbackCall struct {
	interaction string
	token       string
	body        map[string]any
}

type patchCall struct {
	channel string
	message string
	body    map[string]any
}

type approvalStub struct {
	srv       *httptest.Server
	mu        sync.Mutex
	posts     []cardPost
	callbacks []callbackCall
	patches   []patchCall
	postID    string
	postCode  int
}

func newApprovalStub(t *testing.T) *approvalStub {
	t.Helper()
	stub := &approvalStub{postID: "card-7", postCode: http.StatusOK}
	stub.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, approvalHTTPBytes))
		_ = r.Body.Close()
		var decoded map[string]any
		_ = json.Unmarshal(body, &decoded)
		path := strings.Trim(r.URL.Path, "/")
		segments := strings.Split(path, "/")
		stub.mu.Lock()
		defer stub.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && len(segments) == 3 && segments[0] == "channels" && segments[2] == "messages":
			stub.posts = append(stub.posts, cardPost{channel: segments[1], body: decoded})
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(stub.postCode)
			if stub.postCode == http.StatusOK {
				_, _ = w.Write([]byte(`{"id":"` + stub.postID + `"}`))
			}
		case r.Method == http.MethodPost && len(segments) == 4 && segments[0] == "interactions" && segments[3] == "callback":
			stub.callbacks = append(stub.callbacks, callbackCall{interaction: segments[1], token: segments[2], body: decoded})
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPatch && len(segments) == 4 && segments[0] == "channels" && segments[2] == "messages":
			stub.patches = append(stub.patches, patchCall{channel: segments[1], message: segments[3], body: decoded})
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(stub.srv.Close)
	return stub
}

func (s *approvalStub) counts() (posts, callbacks, patches int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.posts), len(s.callbacks), len(s.patches)
}

func newApprovalAdapter(t *testing.T, stub *approvalStub, logs *logCapture) *Adapter {
	t.Helper()
	t.Setenv("AURA_DISCORD_BOT_TOKEN", testToken)
	if logs == nil {
		logs = &logCapture{}
	}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	cfg := testAdapterConfig()
	adapter, err := New(&cfg, &memoryResumeStore{}, newFakeEffectRunner(), &fakeMediaStore{}, &fakeSessionEnsurer{}, logger)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	adapter.restBase = stub.srv.URL
	adapter.rememberTarget("dm:111", approvalBinding{principal: "111", channelID: "333"})
	adapter.rememberTarget("channel:333", approvalBinding{principal: "111", guildID: "222", channelID: "333"})
	return adapter
}

func testPrompt(session string, expiresAt time.Time) *ApprovalPrompt {
	return &ApprovalPrompt{
		ToolName:      "shell.exec",
		ToolVersion:   "1",
		SessionID:     session,
		TurnID:        "turn-1",
		PrincipalID:   "111",
		Arguments:     `{"command":"ls"}`,
		PolicyVersion: "v3",
		ReasonCode:    "approval_required",
		ExpiresAt:     expiresAt,
	}
}

type decideResult struct {
	accepted bool
	err      error
}

func decideAsync(ctx context.Context, adapter *Adapter, prompt *ApprovalPrompt) <-chan decideResult {
	out := make(chan decideResult, 1)
	go func() {
		accepted, err := adapter.Decide(ctx, prompt)
		out <- decideResult{accepted: accepted, err: err}
	}()
	return out
}

func (s *approvalStub) waitPost(t *testing.T, timeout time.Duration) cardPost {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		if len(s.posts) > 0 {
			post := s.posts[len(s.posts)-1]
			s.mu.Unlock()
			return post
		}
		s.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for approval card post")
	return cardPost{}
}

func cardCustomIDs(t *testing.T, post cardPost) (approve, reject string) {
	t.Helper()
	components, _ := post.body["components"].([]any)
	if len(components) != 1 {
		t.Fatalf("card components = %v, want one action row", post.body["components"])
	}
	row, _ := components[0].(map[string]any)
	buttons, _ := row["components"].([]any)
	if len(buttons) != 2 {
		t.Fatalf("card buttons = %v, want approve and reject", row["components"])
	}
	first, _ := buttons[0].(map[string]any)
	second, _ := buttons[1].(map[string]any)
	approve, _ = first["custom_id"].(string)
	reject, _ = second["custom_id"].(string)
	if approve == "" || reject == "" || approve == reject {
		t.Fatalf("card custom ids missing or identical: %q %q", approve, reject)
	}
	return approve, reject
}

func clickPayload(id, userID, guildID, channelID, customID string) *interactionPayload {
	interaction := &interactionPayload{
		ID:        id,
		Token:     "interaction-token-" + id,
		Type:      interactionMessageComponent,
		GuildID:   guildID,
		ChannelID: channelID,
		Data:      interactionData{CustomID: customID, ComponentType: componentButton},
	}
	if guildID != "" {
		interaction.Member = &interactionMember{User: userPayload{ID: userID}}
	} else {
		interaction.User = &userPayload{ID: userID}
	}
	return interaction
}

func awaitDecide(t *testing.T, result <-chan decideResult, timeout time.Duration) decideResult {
	t.Helper()
	select {
	case outcome := <-result:
		return outcome
	case <-time.After(timeout):
		t.Fatal("timed out waiting for approval decision")
		return decideResult{}
	}
}

func TestDecide_ApproveClickAccepts(t *testing.T) {
	stub := newApprovalStub(t)
	logs := &logCapture{}
	adapter := newApprovalAdapter(t, stub, logs)
	result := decideAsync(t.Context(), adapter, testPrompt("dm:111", time.Now().Add(time.Minute)))
	approve, _ := cardCustomIDs(t, stub.waitPost(t, 5*time.Second))
	adapter.resolveComponentClick(t.Context(), clickPayload("ix-1", "111", "", "333", approve))
	outcome := awaitDecide(t, result, 5*time.Second)
	if outcome.err != nil || !outcome.accepted {
		t.Errorf("Decide() = %v, %v; want accepted", outcome.accepted, outcome.err)
	}
	if posts, callbacks, patches := stub.counts(); posts != 1 || callbacks != 1 || patches != 1 {
		t.Errorf("posts = %d, callbacks = %d, patches = %d; want 1 each", posts, callbacks, patches)
	}
	if body := stub.callbacks[0].body; body["type"] != float64(callbackDeferredUpdate) {
		t.Errorf("callback type = %v, want deferred update", body["type"])
	}
	if stub.patches[0].message != "card-7" {
		t.Errorf("disabled message = %q, want card-7", stub.patches[0].message)
	}
	if strings.Contains(logs.String(), testToken) {
		t.Error("bot token leaked into logs")
	}
}

func TestDecide_RejectClickDenies(t *testing.T) {
	stub := newApprovalStub(t)
	adapter := newApprovalAdapter(t, stub, nil)
	result := decideAsync(t.Context(), adapter, testPrompt("channel:333", time.Now().Add(time.Minute)))
	_, reject := cardCustomIDs(t, stub.waitPost(t, 5*time.Second))
	adapter.resolveComponentClick(t.Context(), clickPayload("ix-2", "111", "222", "333", reject))
	outcome := awaitDecide(t, result, 5*time.Second)
	if outcome.err != nil || outcome.accepted {
		t.Errorf("Decide() = %v, %v; want denied", outcome.accepted, outcome.err)
	}
}

func TestDecide_UnknownConversationDeniesWithoutCard(t *testing.T) {
	stub := newApprovalStub(t)
	adapter := newApprovalAdapter(t, stub, nil)
	accepted, err := adapter.Decide(t.Context(), testPrompt("dm:999", time.Now().Add(time.Minute)))
	if err != nil || accepted {
		t.Errorf("Decide() = %v, %v; want fail-closed deny", accepted, err)
	}
	if posts, _, _ := stub.counts(); posts != 0 {
		t.Errorf("card posts = %d, want none without a bound conversation", posts)
	}
}

func TestDecide_ExpiredPromptDeniesWithoutCard(t *testing.T) {
	stub := newApprovalStub(t)
	adapter := newApprovalAdapter(t, stub, nil)
	accepted, err := adapter.Decide(t.Context(), testPrompt("dm:111", time.Now().Add(-time.Minute)))
	if err != nil || accepted {
		t.Errorf("Decide() = %v, %v; want expired deny", accepted, err)
	}
	if posts, _, _ := stub.counts(); posts != 0 {
		t.Errorf("card posts = %d, want none for an expired prompt", posts)
	}
}

func TestDecide_RejectsInvalidPrompt(t *testing.T) {
	stub := newApprovalStub(t)
	adapter := newApprovalAdapter(t, stub, nil)
	var nilCtx context.Context
	if _, err := adapter.Decide(nilCtx, testPrompt("dm:111", time.Now().Add(time.Minute))); err == nil {
		t.Error("nil context accepted")
	}
	if _, err := adapter.Decide(t.Context(), nil); err == nil {
		t.Error("nil prompt accepted")
	}
	prompt := testPrompt("dm:111", time.Now().Add(time.Minute))
	prompt.PrincipalID = ""
	if _, err := adapter.Decide(t.Context(), prompt); err == nil {
		t.Error("prompt without principal accepted")
	}
}

func TestDecide_TimeoutDisablesCardAndDenies(t *testing.T) {
	stub := newApprovalStub(t)
	adapter := newApprovalAdapter(t, stub, nil)
	result := decideAsync(t.Context(), adapter, testPrompt("dm:111", time.Now().Add(60*time.Millisecond)))
	stub.waitPost(t, 5*time.Second)
	outcome := awaitDecide(t, result, 5*time.Second)
	if outcome.err != nil || outcome.accepted {
		t.Errorf("Decide() = %v, %v; want timeout deny", outcome.accepted, outcome.err)
	}
	if _, _, patches := stub.counts(); patches != 1 {
		t.Errorf("disable patches = %d, want 1 after timeout", patches)
	}
}

func TestDecide_CancelFailsClosed(t *testing.T) {
	stub := newApprovalStub(t)
	adapter := newApprovalAdapter(t, stub, nil)
	ctx, cancel := context.WithCancel(t.Context())
	result := decideAsync(ctx, adapter, testPrompt("dm:111", time.Now().Add(time.Minute)))
	approve, _ := cardCustomIDs(t, stub.waitPost(t, 5*time.Second))
	cancel()
	outcome := awaitDecide(t, result, 5*time.Second)
	if outcome.err == nil {
		t.Error("cancelled Decide() returned nil error")
	}
	if outcome.accepted {
		t.Error("cancelled Decide() accepted")
	}
	adapter.resolveComponentClick(t.Context(), clickPayload("ix-late", "111", "", "333", approve))
	if _, callbacks, _ := stub.counts(); callbacks != 1 {
		t.Errorf("callbacks = %d, want only the late-click silent ack", callbacks)
	}
}

func TestDecide_CardPostFailureDenies(t *testing.T) {
	stub := newApprovalStub(t)
	stub.postCode = http.StatusInternalServerError
	adapter := newApprovalAdapter(t, stub, nil)
	accepted, err := adapter.Decide(t.Context(), testPrompt("dm:111", time.Now().Add(time.Minute)))
	if err != nil || accepted {
		t.Errorf("Decide() = %v, %v; want deny when the card cannot post", accepted, err)
	}
	adapter.approvalsMu.Lock()
	pending := len(adapter.approvals)
	adapter.approvalsMu.Unlock()
	if pending != 0 {
		t.Errorf("pending approvals = %d, want none after card failure", pending)
	}
}

func TestApproval_ReplayAfterDecisionRejected(t *testing.T) {
	stub := newApprovalStub(t)
	adapter := newApprovalAdapter(t, stub, nil)
	result := decideAsync(t.Context(), adapter, testPrompt("dm:111", time.Now().Add(time.Minute)))
	approve, _ := cardCustomIDs(t, stub.waitPost(t, 5*time.Second))
	adapter.resolveComponentClick(t.Context(), clickPayload("ix-1", "111", "", "333", approve))
	if outcome := awaitDecide(t, result, 5*time.Second); !outcome.accepted {
		t.Fatal("first click did not accept")
	}
	adapter.resolveComponentClick(t.Context(), clickPayload("ix-2", "111", "", "333", approve))
	if _, callbacks, patches := stub.counts(); callbacks != 2 || patches != 1 {
		t.Errorf("callbacks = %d, patches = %d; want replay absorbed with no second disable", callbacks, patches)
	}
}

func TestApproval_DuplicateInteractionIDAbsorbed(t *testing.T) {
	stub := newApprovalStub(t)
	adapter := newApprovalAdapter(t, stub, nil)
	result := decideAsync(t.Context(), adapter, testPrompt("dm:111", time.Now().Add(time.Minute)))
	_, reject := cardCustomIDs(t, stub.waitPost(t, 5*time.Second))
	click := clickPayload("ix-dup", "999", "", "333", reject)
	adapter.resolveComponentClick(t.Context(), click)
	adapter.resolveComponentClick(t.Context(), click)
	select {
	case outcome := <-result:
		t.Fatalf("duplicate rejected clicks decided: %+v", outcome)
	case <-time.After(100 * time.Millisecond):
	}
	if _, callbacks, patches := stub.counts(); callbacks != 2 || patches != 0 {
		t.Errorf("callbacks = %d, patches = %d; want duplicate absorbed with no disable", callbacks, patches)
	}
	approve, _ := cardCustomIDs(t, stub.waitPost(t, 5*time.Second))
	adapter.resolveComponentClick(t.Context(), clickPayload("ix-real", "111", "", "333", approve))
	if outcome := awaitDecide(t, result, 5*time.Second); !outcome.accepted {
		t.Errorf("valid click after duplicates = %v, want accepted", outcome.accepted)
	}
}

func TestApproval_MutatedTokenRejectedWithoutConsuming(t *testing.T) {
	stub := newApprovalStub(t)
	adapter := newApprovalAdapter(t, stub, nil)
	result := decideAsync(t.Context(), adapter, testPrompt("dm:111", time.Now().Add(time.Minute)))
	approve, _ := cardCustomIDs(t, stub.waitPost(t, 5*time.Second))
	mutated := approve[:len(approve)-1] + flipHex(approve[len(approve)-1])
	adapter.resolveComponentClick(t.Context(), clickPayload("ix-bad", "111", "", "333", mutated))
	select {
	case outcome := <-result:
		t.Fatalf("mutated token decided: %+v", outcome)
	case <-time.After(100 * time.Millisecond):
	}
	adapter.resolveComponentClick(t.Context(), clickPayload("ix-good", "111", "", "333", approve))
	if outcome := awaitDecide(t, result, 5*time.Second); !outcome.accepted {
		t.Errorf("valid click after mutation = %v, want accepted", outcome.accepted)
	}
}

func flipHex(last byte) string {
	if last == 'a' {
		return "b"
	}
	return "a"
}

func TestApproval_WrongChannelRejected(t *testing.T) {
	stub := newApprovalStub(t)
	adapter := newApprovalAdapter(t, stub, nil)
	result := decideAsync(t.Context(), adapter, testPrompt("channel:333", time.Now().Add(time.Minute)))
	approve, _ := cardCustomIDs(t, stub.waitPost(t, 5*time.Second))
	adapter.resolveComponentClick(t.Context(), clickPayload("ix-x", "111", "222", "999", approve))
	select {
	case outcome := <-result:
		t.Fatalf("cross-channel click decided: %+v", outcome)
	case <-time.After(100 * time.Millisecond):
	}
	adapter.resolveComponentClick(t.Context(), clickPayload("ix-ok", "111", "222", "333", approve))
	if outcome := awaitDecide(t, result, 5*time.Second); !outcome.accepted {
		t.Errorf("correct-channel click = %v, want accepted", outcome.accepted)
	}
}

func TestApproval_UnknownTokenRejected(t *testing.T) {
	stub := newApprovalStub(t)
	adapter := newApprovalAdapter(t, stub, nil)
	adapter.resolveComponentClick(t.Context(), clickPayload("ix-ghost", "111", "", "333", "aura:0123456789abcdef:a:0123456789abcdef"))
	if _, callbacks, patches := stub.counts(); callbacks != 1 || patches != 0 {
		t.Errorf("callbacks = %d, patches = %d; want silent reject with no disable", callbacks, patches)
	}
}

func TestApproval_ConcurrentClicksOneWins(t *testing.T) {
	stub := newApprovalStub(t)
	adapter := newApprovalAdapter(t, stub, nil)
	result := decideAsync(t.Context(), adapter, testPrompt("dm:111", time.Now().Add(time.Minute)))
	approve, reject := cardCustomIDs(t, stub.waitPost(t, 5*time.Second))
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			custom := approve
			if i%2 == 1 {
				custom = reject
			}
			adapter.resolveComponentClick(context.Background(), clickPayload("ix-"+string(rune('a'+i)), "111", "", "333", custom))
		}()
	}
	wg.Wait()
	outcome := awaitDecide(t, result, 5*time.Second)
	if outcome.err != nil {
		t.Fatalf("Decide(): %v", outcome.err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, callbacks, patches := stub.counts(); callbacks != 16 || patches != 1 {
		t.Errorf("callbacks = %d, patches = %d; want every click acked with one disable", callbacks, patches)
	}
}

func TestParseApprovalCustomID(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		valid bool
	}{
		{"empty", "", false},
		{"wrong prefix", "bogus:0123456789abcdef:a:0123456789abcdef", false},
		{"short token", "aura:abc:a:0123456789abcdef", false},
		{"bad action", "aura:0123456789abcdef:x:0123456789abcdef", false},
		{"non-hex mac", "aura:0123456789abcdef:a:zzzzzzzzzzzzzzzz", false},
		{"approve", "aura:0123456789abcdef:a:0123456789abcdef", true},
		{"reject", "aura:0123456789abcdef:r:fedcba9876543210", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, ok := parseApprovalCustomID(tc.raw)
			if ok != tc.valid {
				t.Errorf("parse(%q) valid = %v, want %v", tc.raw, ok, tc.valid)
			}
		})
	}
}

func TestRenderApprovalCardBoundsArguments(t *testing.T) {
	prompt := testPrompt("dm:111", time.Now().Add(time.Minute))
	prompt.Arguments = "@everyone " + strings.Repeat("x", approvalCardArgRunes+100)
	card := renderApprovalCard(prompt)
	if strings.Contains(card, "@everyone") {
		t.Error("card preserves a mass mention")
	}
	if len([]rune(card)) > approvalCardArgRunes+512 {
		t.Errorf("card runes = %d, want bounded arguments", len([]rune(card)))
	}
}
