package toolbroker

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn/store"
)

func TestRequestDigestAndEffectPayloadAreUnchangedWithoutAPreview(t *testing.T) {
	request := &ToolRequest{
		RequestID: "call-1", TurnID: "turn-1", SessionID: "session-1", PrincipalID: "owner-1",
		ToolName: "read_file", ToolVersion: "v1", Arguments: json.RawMessage(`{"path":"a.txt"}`),
		Capabilities: []string{"workspace-read"}, Trust: approval.TrustOwnerInput,
		IdempotencyKey: "adk/session-1/turn-1/call-1-0",
	}
	const wantDigest = "2a42a26d7a3782cac79e0538030ea54fa650cc33dcdd800fef0ce38d83444b9e"
	const wantPayload = `{"tool_name":"read_file","tool_version":"v1","arguments":{"path":"a.txt"}}`
	digest, err := requestDigest(request, "builtin-tools-v1")
	if err != nil || digest != wantDigest {
		t.Errorf("request digest = %s (%v), want %s", digest, err, wantDigest)
	}
	payload, err := effectPayload(request)
	if err != nil || string(payload) != wantPayload {
		t.Errorf("effect payload = %s (%v), want %s", payload, err, wantPayload)
	}
	request.Origin, request.ContentTaintTracked = "terminal", true
	if again, _ := requestDigest(request, "builtin-tools-v1"); again != wantDigest {
		t.Errorf("origin and the taint tracking flag changed the request digest to %s", again)
	}
}

func legacyPrompt(tool, arguments, reason string) ApprovalPrompt {
	return ApprovalPrompt{
		ToolName: tool, ToolVersion: "v1", SessionID: "session-1", TurnID: "turn-1", PrincipalID: "owner-1",
		Arguments: arguments, Timeout: 30 * time.Second, MaxOutputBytes: 64 * 1024, PolicyVersion: "builtin-tools-v1",
		ReasonCode: reason, StandingGrantAllowed: true,
	}
}

func TestACallerCannotSetThePreviewDigest(t *testing.T) {
	rig := newOperationRig(t, openOperationDatabase(t), &operationOptions{reachable: true}, readStub)
	plain := operationRequest(readStub.name, "x")
	preset := operationRequest(readStub.name, "x")
	preset.PreviewDigest = "chosen-by-the-caller"
	first, err := rig.broker.canonicalRequest(plain)
	if err != nil {
		t.Fatalf("canonical request: %v", err)
	}
	second, err := rig.broker.canonicalRequest(preset)
	if err != nil {
		t.Fatalf("canonical request: %v", err)
	}
	if first.RequestDigest != second.RequestDigest || second.PreviewDigest != "" {
		t.Errorf("a preview digest set by the caller survived: %+v", second)
	}
}

type legacyCall struct {
	name         string
	tool         string
	arguments    string
	capabilities []string
	tainted      bool
	actions      string
	states       []string
	prompts      []ApprovalPrompt
	class        ResultClass
	output       string
}

func TestLegacyToolsKeepTheirJournaledPathPromptsAndResults(t *testing.T) {
	const quiet = "call-1-0/policy,call-1-0/authorization,call-1-0/execution"
	cases := []legacyCall{
		{name: "a plain read", tool: "read_file", arguments: `{"path":"a.txt"}`, capabilities: []string{"workspace-read"}, actions: quiet, states: []string{ApprovalAuto, ApprovalAuto}, class: ResultOK, output: `{"ok":true}`},
		{
			name: "an effectful tool that asks", tool: "exec", arguments: `{"command":["true"]}`, capabilities: []string{"exec-linux"}, actions: quiet,
			states: []string{ApprovalApproved, ApprovalApproved}, class: ResultOK, output: `{"ok":true}`,
			prompts: []ApprovalPrompt{legacyPrompt("exec", `{"command":["true"]}`, "approval_required")},
		},
		{name: "a sink on a clean turn", tool: "web_fetch", arguments: `{"url":"https://example.com"}`, capabilities: []string{"public-web"}, actions: quiet, states: []string{ApprovalAuto, ApprovalAuto}, class: ResultOK, output: `{"ok":true}`},
		{
			name: "a sink on a tainted turn", tool: "web_fetch", arguments: `{"url":"https://example.com"}`, capabilities: []string{"public-web"}, tainted: true, actions: quiet,
			states: []string{ApprovalApproved, ApprovalApproved}, class: ResultOK, output: `{"ok":true}`,
			prompts: []ApprovalPrompt{func() ApprovalPrompt {
				p := legacyPrompt("web_fetch", `{"url":"https://example.com"}`, "untrusted_input")
				p.TaintSource = "content from outside your control"
				return p
			}()},
		},
		{
			name: "a tool that requires approval", tool: "write_file", arguments: `{"path":"a.txt","content":"hello"}`, capabilities: []string{"workspace-write"}, actions: quiet,
			states: []string{ApprovalApproved, ApprovalApproved}, class: ResultOK, output: `{"ok":true}`,
			prompts: []ApprovalPrompt{legacyPrompt("write_file", `{"content":"hello","path":"a.txt"}`, "approval_required")},
		},
		{name: "an orchestration tool", tool: "workflow_list", arguments: `{}`, actions: quiet, states: []string{ApprovalAuto, ApprovalAuto}, class: ResultOK, output: `{"ok":true}`},
	}
	hosted := startHostedRuntime(t)
	seedSession(t, hosted.db)
	journal, err := effect.NewJournal(hosted.db, effect.Options{Logger: slog.New(slog.DiscardHandler), Turns: turnstore.NewTurnStore(hosted.db)})
	if err != nil {
		t.Fatalf("new journal: %v", err)
	}
	executor, err := effect.NewExecutor(journal)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var runs atomic.Int32
			observed := &observedApprovals{}
			decider := &scriptedDecider{answer: approveEvery}
			adapters := map[string]Adapter{}
			for _, definition := range fixtureDefinitions() {
				adapters[definition.Name+"@v1"] = func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
					runs.Add(1)
					return ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
				}
			}
			broker, err := newFixtureBrokerOrErr(t, &Options{
				HostCapabilities: capabilitytest.Host(t), Effects: executor, Observer: observed.observe, Adapters: adapters,
				ApprovalDecider: decider.decide, ApprovalReachable: func(string) bool { return true },
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			request := classifiedRequest(tc.tool, tc.arguments, principal.ClassOwner, tc.capabilities...)
			request.Tainted, request.PrincipalID, request.EventSequence = tc.tainted, "owner-1", 0
			request.AllocateEventSequence, request.EventInvocation, request.EventBranch, request.EventAuthor = true, "invocation-1", "main", "test"
			request.IdempotencyKey = "idempotency-" + strings.ReplaceAll(tc.name, " ", "-")
			var first, replay callOutcome
			key := hosted.run(t, func(ctx context.Context, inv durable.Invocation) error {
				first = outcomeOf(broker.Execute(inTurn(ctx, inv), request))
				replay = outcomeOf(broker.Execute(inTurn(ctx, inv), request))
				return nil
			})
			want := callOutcome{class: tc.class, output: tc.output}
			if first != want || replay != want {
				t.Errorf("first = %+v, replay = %+v, want %+v for both", first, replay, want)
			}
			if got := joinedKeys(hosted.actionRows(t, key)); got != tc.actions {
				t.Errorf("action keys = %s, want %s", got, tc.actions)
			}
			requireStates(t, observed.list(), tc.states...)
			prompts := decider.asked()
			if len(prompts) != len(tc.prompts) {
				t.Fatalf("approval prompts = %d, want %d", len(prompts), len(tc.prompts))
			}
			for i := range prompts {
				prompts[i].ExpiresAt = time.Time{}
				if prompts[i] != tc.prompts[i] {
					t.Errorf("prompt = %+v, want %+v", prompts[i], tc.prompts[i])
				}
			}
			intents, err := executor.Journal().ListByState(t.Context(), effect.StateSucceeded, 0)
			if err != nil {
				t.Fatalf("list intents: %v", err)
			}
			for i := range intents {
				if intents[i].Provider != effectProvider {
					t.Errorf("intent provider = %q, want %q", intents[i].Provider, effectProvider)
				}
			}
		})
	}
}
