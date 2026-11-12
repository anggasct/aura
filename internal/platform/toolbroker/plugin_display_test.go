package toolbroker

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/tools/displaytest"
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

func TestDurableApprovalCarriesTheSameDisplayAndFailsClosedWhenItCannotFit(t *testing.T) {
	hosted := startHostedRuntime(t)
	t.Run("the display text reaches the decider", func(t *testing.T) {
		arguments := `{"body":"` + strings.Repeat("b", 3000) + `","to":["a@mail.example"],"subject":"Hi"}`
		inMemory := newDisplayHarness(t, []string{"to", "subject", "body"})
		if err := inMemory.run(arguments, principal.ClassTrusted); err != nil {
			t.Fatalf("in-memory Execute: %v", err)
		}
		h := newDisplayHarness(t, []string{"to", "subject", "body"})
		var durableErr error
		hosted.run(t, func(ctx context.Context, inv durable.Invocation) error {
			_, durableErr = h.broker.Execute(inTurn(ctx, inv), classifiedRequest(displayToolName, arguments, principal.ClassTrusted))
			return nil
		})
		if durableErr != nil {
			t.Fatalf("durable Execute: %v", durableErr)
		}
		prompts, want := h.promptList(), inMemory.promptList()
		if len(prompts) != 1 || len(want) != 1 || prompts[0].Arguments != want[0].Arguments {
			t.Fatalf("durable prompts = %+v, want the in-memory display %+v", prompts, want)
		}
		display := prompts[0].Arguments
		if !strings.HasPrefix(display, "to: [\"a@mail.example\"]\nsubject: Hi\nbody: ") || !strings.Contains(display, "bytes hidden]") || len(display) > tools.ApprovalDisplayLimit {
			t.Fatalf("display = %q", display)
		}
		if h.calls != 1 {
			t.Fatalf("calls = %d, want 1", h.calls)
		}
	})
	t.Run("an oversized display never reaches the decider", func(t *testing.T) {
		h := newDisplayHarness(t, []string{"body", "to"})
		var durableErr error
		hosted.run(t, func(ctx context.Context, inv durable.Invocation) error {
			_, durableErr = h.broker.Execute(inTurn(ctx, inv), classifiedRequest(displayToolName, `{"body":"`+strings.Repeat("b", 3000)+`","to":"a@mail.example"}`, principal.ClassTrusted))
			return nil
		})
		if classOf(durableErr) != ResultApprovalDisplayTooLarge {
			t.Fatalf("Execute error = %v, want approval_display_too_large", durableErr)
		}
		if len(h.promptList()) != 0 || h.calls != 0 {
			t.Fatalf("prompts = %d calls = %d, want neither", len(h.promptList()), h.calls)
		}
	})
}
