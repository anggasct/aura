package toolbroker

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/tools/displaytest"
	"github.com/anggasct/aura/internal/store"
)

const displayToolName = "plugin_mailer_send"

type displayHarness struct {
	broker  *Broker
	access  *fakeAccess
	mu      sync.Mutex
	prompts []*ApprovalPrompt
	calls   int
}

func (h *displayHarness) promptList() []*ApprovalPrompt {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*ApprovalPrompt(nil), h.prompts...)
}

func strictObjectValidator(raw json.RawMessage) (json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	return raw, nil
}

func newDisplayHarness(t *testing.T, display []string, secrets ...string) *displayHarness {
	t.Helper()
	h := &displayHarness{access: teamAccess()}
	h.access.ceilings[principal.ClassTrusted] = append(h.access.ceilings[principal.ClassTrusted], "plugin_mailer")
	definition := displaytest.Definition(display)
	definition.Schema = json.RawMessage(`{"type":"object"}`)
	definition.RequiresApproval = true
	definition.Validator = strictObjectValidator
	broker, err := New(&Options{
		HostCapabilities: capabilitytest.Host(t), Effects: newBrokerEffectExecutor(t),
		Secrets: secrets,
		Access:  h.access,
		ApprovalDecider: func(_ context.Context, prompt *ApprovalPrompt) (bool, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.prompts = append(h.prompts, prompt)
			return true, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rule := approval.Rule{
		ToolName: displayToolName, ToolVersion: "v1", RequiresApproval: true,
		AllowedTrust: []approval.TrustLabel{approval.TrustOwnerInput, approval.TrustDerivedUntrusted},
	}
	adapter := func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.calls++
		return ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
	}
	if err := broker.RegisterTool(definition, adapter, &rule); err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}
	h.broker = broker
	return h
}

func (h *displayHarness) run(arguments string, class principal.Class) error {
	request := classifiedRequest(displayToolName, arguments, class)
	_, err := h.broker.Execute(context.Background(), request)
	return err
}

func TestPluginEffectApprovalShowsTheFieldsForEveryScenario(t *testing.T) {
	for _, scenario := range displaytest.Scenarios() {
		t.Run(scenario.Name, func(t *testing.T) {
			h := newDisplayHarness(t, scenario.Display)
			if err := h.run(scenario.Arguments, principal.ClassTrusted); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			prompts := h.promptList()
			if len(prompts) != 1 {
				t.Fatalf("prompts = %d, want 1", len(prompts))
			}
			if want := scenario.Text(t); prompts[0].Arguments != want {
				t.Fatalf("prompt arguments = %q, want %q", prompts[0].Arguments, want)
			}
			scenario.Check(t, prompts[0].Arguments, false)
		})
	}
}

func TestPluginEffectToolsNeverUseStandingGrants(t *testing.T) {
	h := newDisplayHarness(t, []string{"to", "subject", "body"})
	h.access.standing = true
	for i := range 3 {
		request := classifiedRequest(displayToolName, `{"to":"a@mail.example","subject":"s","body":"b"}`, principal.ClassTrusted)
		request.RequestID, request.IdempotencyKey, request.EventSequence = "request-"+string(rune('a'+i)), "key-"+string(rune('a'+i)), uint64(i+1)
		if _, err := h.broker.Execute(context.Background(), request); err != nil {
			t.Fatalf("Execute %d: %v", i, err)
		}
	}
	if got := len(h.promptList()); got != 3 || h.calls != 3 {
		t.Fatalf("prompts = %d calls = %d, want a card for every call", got, h.calls)
	}
	if h.access.lookups != 0 {
		t.Fatalf("standing grant lookups = %d, want none for a plugin effect tool", h.access.lookups)
	}
}

func TestADisplayThatCannotFitFailsClosedBeforeAnyPromptOrCall(t *testing.T) {
	cases := []struct {
		name      string
		display   []string
		arguments string
	}{
		{"a long field before the last", []string{"body", "to"}, `{"body":"` + strings.Repeat("b", 2000) + `","to":"a@mail.example"}`},
		{"a long array", []string{"to"}, `{"to":[` + strings.Repeat(`"recipient@mail.example",`, 100) + `"z@mail.example"]}`},
		{"a long undeclared field", []string{"to"}, `{"to":"a@mail.example","extra":"` + strings.Repeat("e", 2000) + `"}`},
		{"no display and a long field", nil, `{"body":"` + strings.Repeat("b", 2000) + `"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newDisplayHarness(t, tc.display)
			err := h.run(tc.arguments, principal.ClassTrusted)
			if classOf(err) != ResultApprovalDisplayTooLarge {
				t.Fatalf("Execute error = %v, want approval_display_too_large", err)
			}
			if len(h.promptList()) != 0 || h.calls != 0 {
				t.Fatalf("prompts = %d calls = %d, want neither", len(h.promptList()), h.calls)
			}
		})
	}
}

func TestDuplicateAndCaseVariantKeysNeverReachApprovalOrExecution(t *testing.T) {
	cases := []struct{ name, arguments string }{
		{"duplicate", `{"to":"a@mail.example","to":"attacker@evil.example"}`},
		{"case variant", `{"to":"a@mail.example","TO":"attacker@evil.example"}`},
		{"nested duplicate", `{"to":"a@mail.example","meta":{"k":1,"k":2}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newDisplayHarness(t, []string{"to"})
			err := h.run(tc.arguments, principal.ClassTrusted)
			if classOf(err) != ResultInvalidArgument {
				t.Fatalf("Execute error = %v, want invalid_argument", err)
			}
			if len(h.promptList()) != 0 || h.calls != 0 {
				t.Fatalf("prompts = %d calls = %d, want neither", len(h.promptList()), h.calls)
			}
		})
	}
}

func TestDuplicateKeysAreRejectedForBuiltinToolsToo(t *testing.T) {
	broker, err := New(&Options{HostCapabilities: capabilitytest.Host(t), Effects: newBrokerEffectExecutor(t), Adapters: map[string]Adapter{
		"read_file@v1": func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
			return ToolResult{Output: json.RawMessage(`{}`)}, nil
		},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	request := brokerRequest("read_file", `{"path":"a.txt","path":"/etc/passwd"}`, "workspace-read")
	if _, err := broker.Execute(context.Background(), request); classOf(err) != ResultInvalidArgument {
		t.Fatalf("Execute error = %v, want invalid_argument", err)
	}
}

func TestPluginApprovalDisplayIsRedactedBeforeItIsSized(t *testing.T) {
	h := newDisplayHarness(t, []string{"to", "body"}, "sk-canary-9f2a")
	if err := h.run(`{"to":"a@mail.example","body":"token sk-canary-9f2a end"}`, principal.ClassTrusted); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	display := h.promptList()[0].Arguments
	if strings.Contains(display, "sk-canary-9f2a") || !strings.Contains(display, "token [REDACTED] end") {
		t.Fatalf("display = %q", display)
	}
}

func TestOtherToolsKeepTheirJSONRenderingWithEscapedControlCharacters(t *testing.T) {
	var prompted *ApprovalPrompt
	broker, err := New(&Options{
		HostCapabilities: capabilitytest.Host(t), Effects: newBrokerEffectExecutor(t),
		Adapters: map[string]Adapter{
			"exec@v1": func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
				return ToolResult{Output: json.RawMessage(`{}`)}, nil
			},
		},
		ApprovalDecider: func(_ context.Context, prompt *ApprovalPrompt) (bool, error) {
			prompted = prompt
			return false, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	request := brokerRequest("exec", `{"command":["echo","a`+string(rune(0x202e))+`b`+string(rune(0x85))+`"]}`, "exec-linux")
	request.EventSequence = 1
	if _, err := broker.Execute(context.Background(), request); classOf(err) != ResultApprovalRequired {
		t.Fatalf("Execute error = %v, want approval_required", err)
	}
	if prompted == nil || prompted.Arguments != `{"command":["echo","a\u{202e}b\u{0085}"]}` {
		t.Fatalf("prompt = %+v", prompted)
	}
}

func startDurableScope(t *testing.T) (context.Context, *recordingSink) {
	t.Helper()
	backend := durable.NewFake()
	invocations := make(chan durable.Invocation, 1)
	backend.RegisterHandler("driver", func(_ context.Context, inv durable.Invocation) error {
		invocations <- inv
		select {}
	})
	if _, err := backend.Start(context.Background(), durable.StartRequest{Handler: "driver", Key: "turn-1"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	var inv durable.Invocation
	select {
	case inv = <-invocations:
	case <-time.After(20 * time.Second):
		t.Fatal("driver invocation never started")
	}
	sink := &recordingSink{ready: make(chan struct{})}
	ctx := WithApprovalSink(context.Background(), sink)
	return durable.WithTurnScope(ctx, durable.NewTurnScope(inv)), sink
}

func TestDurableApprovalCarriesTheSameDisplayAndFailsClosedWhenItCannotFit(t *testing.T) {
	t.Run("display text reaches the event", func(t *testing.T) {
		h := newDisplayHarness(t, []string{"to", "subject", "body"})
		ctx, sink := startDurableScope(t)
		done := make(chan error, 1)
		go func() {
			_, err := h.broker.Execute(ctx, classifiedRequest(displayToolName, `{"body":"`+strings.Repeat("b", 3000)+`","to":["a@mail.example"],"subject":"Hi"}`, principal.ClassTrusted))
			done <- err
		}()
		select {
		case <-sink.ready:
		case err := <-done:
			t.Fatalf("Execute ended before parking: %v", err)
		case <-time.After(20 * time.Second):
			t.Fatal("approval event never emitted")
		}
		events := sink.snapshot()
		if len(events) != 1 || !strings.HasPrefix(events[0].Arguments, "to: [\"a@mail.example\"]\nsubject: Hi\nbody: ") || !strings.Contains(events[0].Arguments, "bytes hidden]") || len(events[0].Arguments) > tools.ApprovalDisplayLimit {
			t.Fatalf("approval events = %+v", events)
		}
	})
	t.Run("an oversized display never parks", func(t *testing.T) {
		h := newDisplayHarness(t, []string{"body", "to"})
		ctx, sink := startDurableScope(t)
		_, err := h.broker.Execute(ctx, classifiedRequest(displayToolName, `{"body":"`+strings.Repeat("b", 3000)+`","to":"a@mail.example"}`, principal.ClassTrusted))
		if classOf(err) != ResultApprovalDisplayTooLarge {
			t.Fatalf("Execute error = %v, want approval_display_too_large", err)
		}
		if len(sink.snapshot()) != 0 || h.calls != 0 {
			t.Fatalf("events = %d calls = %d, want neither", len(sink.snapshot()), h.calls)
		}
	})
}

type captureEvents struct {
	mu     sync.Mutex
	stored []*store.RuntimeEvent
}

func (c *captureEvents) UpsertEvent(_ context.Context, e *store.RuntimeEvent) (sequence uint64, inserted bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stored = append(c.stored, e)
	return 1, true, nil
}

func TestApprovalEventPayloadCarriesTheDisplayAsText(t *testing.T) {
	events := &captureEvents{}
	sink := NewApprovalEventSink(events, func(*store.RuntimeEvent) {})
	err := sink.EmitApprovalRequired(context.Background(), &ApprovalEvent{
		ID: "approval-1", TurnID: "turn-1", SessionID: "session-1", PrincipalID: "owner-1", ToolName: displayToolName, ToolVersion: "v1",
		Arguments: "to: [\"a@mail.example\"]\nbody: hi", ApprovalID: "addr", ExpiresAt: time.Now().Add(time.Minute), CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("EmitApprovalRequired: %v", err)
	}
	var payload struct {
		Arguments string `json:"arguments"`
	}
	if err := json.Unmarshal(events.stored[0].Payload, &payload); err != nil || payload.Arguments != "to: [\"a@mail.example\"]\nbody: hi" {
		t.Fatalf("payload = %s, %v", events.stored[0].Payload, err)
	}
}
