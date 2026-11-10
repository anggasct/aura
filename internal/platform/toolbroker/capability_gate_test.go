package toolbroker

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/tools"
)

const goldenExecRequestDigest = "fd05ada23732a95373285a61121a701d5969ce09e0327a6e73e710881fd91f83"

type gatedTool struct {
	name       string
	arguments  string
	capability string
}

var gatedTools = []gatedTool{
	{name: "exec", arguments: `{"command":["true"]}`, capability: "exec-linux"},
	{name: "read_file", arguments: `{"path":"note.txt"}`, capability: "workspace-read"},
	{name: "list_dir", arguments: `{"path":"."}`, capability: "workspace-read"},
	{name: "write_file", arguments: `{"path":"a.txt","content":"x"}`, capability: "workspace-write"},
	{name: "web_fetch", arguments: `{"url":"https://example.com"}`, capability: "public-web"},
	{name: "web_search", arguments: `{"query":"aura"}`, capability: "provider-search"},
}

type gateProbe struct {
	broker   *Broker
	executor *effect.Executor
	adapter  int
	prompts  int
}

func newGateProbe(t *testing.T, host []string) *gateProbe {
	t.Helper()
	probe := &gateProbe{executor: newBrokerEffectExecutor(t)}
	adapters := map[string]Adapter{}
	for _, tool := range gatedTools {
		adapters[tool.name+"@v1"] = func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
			probe.adapter++
			return ToolResult{Class: ResultOK, Output: json.RawMessage(`{}`)}, nil
		}
	}
	broker, err := New(&Options{
		HostCapabilities: host,
		Effects:          probe.executor,
		Adapters:         adapters,
		ApprovalDecider: func(context.Context, *ApprovalPrompt) (bool, error) {
			probe.prompts++
			return true, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	probe.broker = broker
	return probe
}

func (p *gateProbe) intents(t *testing.T) int {
	t.Helper()
	total := 0
	for _, state := range []effect.State{effect.StatePrepared, effect.StateStarted, effect.StateSucceeded, effect.StateUnknown, effect.StateFailed} {
		listed, err := p.executor.Journal().ListByState(context.Background(), state, 0)
		if err != nil {
			t.Fatalf("ListByState(%s): %v", state, err)
		}
		total += len(listed)
	}
	return total
}

func hostWithout(t *testing.T, capability string) []string {
	t.Helper()
	host := capabilitytest.Host(t)
	return slices.DeleteFunc(host, func(name string) bool { return name == capability })
}

func TestBrokerDeniesToolsWhoseCapabilityTheHostLacks(t *testing.T) {
	for _, tool := range gatedTools {
		hosts := map[string][]string{
			"empty host set":       nil,
			"capability not in it": hostWithout(t, tool.capability),
		}
		for hostName, host := range hosts {
			t.Run(tool.name+"/"+hostName, func(t *testing.T) {
				probe := newGateProbe(t, host)
				request := brokerRequest(tool.name, tool.arguments, tool.capability)
				request.EventSequence = 1
				_, err := probe.broker.Execute(context.Background(), request)
				if code, ok := CodeOf(err); !ok || code != ResultCapabilityUnavailable {
					t.Fatalf("Execute error = %v, want capability_unavailable", err)
				}
				if probe.adapter != 0 || probe.prompts != 0 || probe.intents(t) != 0 {
					t.Fatalf("adapter calls = %d, approval prompts = %d, intents = %d; want none", probe.adapter, probe.prompts, probe.intents(t))
				}
			})
		}
	}
}

func TestBrokerDeniesARequestThatClaimsNothingWhenTheHostLacksTheCapability(t *testing.T) {
	for _, tool := range gatedTools {
		t.Run(tool.name, func(t *testing.T) {
			probe := newGateProbe(t, hostWithout(t, tool.capability))
			request := brokerRequest(tool.name, tool.arguments)
			request.EventSequence = 1
			_, err := probe.broker.Execute(context.Background(), request)
			if code, ok := CodeOf(err); !ok || code != ResultCapabilityUnavailable {
				t.Fatalf("Execute error = %v, want capability_unavailable", err)
			}
			if probe.adapter != 0 || probe.prompts != 0 {
				t.Fatalf("adapter calls = %d, approval prompts = %d; want none", probe.adapter, probe.prompts)
			}
		})
	}
}

func TestBrokerDeniesARequestClaimingACapabilityTheHostLacks(t *testing.T) {
	probe := newGateProbe(t, []string{"workspace-read"})
	request := brokerRequest("read_file", `{"path":"note.txt"}`, "workspace-read", "exec-linux")
	request.EventSequence = 1
	_, err := probe.broker.Execute(context.Background(), request)
	if code, ok := CodeOf(err); !ok || code != ResultCapabilityUnavailable {
		t.Fatalf("Execute error = %v, want capability_unavailable", err)
	}
	if probe.adapter != 0 {
		t.Fatalf("adapter calls = %d, want none", probe.adapter)
	}
}

func TestBrokerRunsToolsWhoseCapabilityTheHostHolds(t *testing.T) {
	for _, tool := range gatedTools {
		t.Run(tool.name, func(t *testing.T) {
			probe := newGateProbe(t, []string{tool.capability})
			request := brokerRequest(tool.name, tool.arguments, tool.capability)
			request.EventSequence = 1
			result, err := probe.broker.Execute(context.Background(), request)
			if err != nil || result.Class != ResultOK {
				t.Fatalf("Execute = %+v, %v; want ok", result, err)
			}
			wantPrompts := 0
			if definition := tools.DefinitionsByKey()[tool.name+"@v1"]; definition.RequiresApproval {
				wantPrompts = 1
			}
			if probe.adapter != 1 || probe.prompts != wantPrompts {
				t.Fatalf("adapter calls = %d, approval prompts = %d; want 1 and %d", probe.adapter, probe.prompts, wantPrompts)
			}
		})
	}
}

func TestBrokerWithoutAHostSetOffersNothingThatNeedsACapability(t *testing.T) {
	probe := newGateProbe(t, nil)
	for _, tool := range gatedTools {
		request := brokerRequest(tool.name, tool.arguments, tool.capability)
		if _, err := probe.broker.Evaluate(context.Background(), request); err == nil {
			t.Errorf("Evaluate(%s) with no host set succeeded", tool.name)
		}
	}
}

func TestRequestDigestIsIndependentOfTheHostSet(t *testing.T) {
	request := &ToolRequest{
		RequestID: "request-1", TurnID: "turn-1", SessionID: "session-1", PrincipalID: "owner-1",
		ToolName: "exec", ToolVersion: "v1", Arguments: json.RawMessage(`{"command":["true"]}`),
		Capabilities: []string{"exec-linux"}, Trust: approval.TrustOwnerInput, IdempotencyKey: "idempotency-1",
	}
	for name, host := range map[string][]string{"full host": capabilitytest.Host(t), "empty host": nil} {
		t.Run(name, func(t *testing.T) {
			broker, err := New(&Options{HostCapabilities: host})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			canonical, err := broker.canonicalRequest(request)
			if err != nil {
				t.Fatalf("canonicalRequest: %v", err)
			}
			if canonical.RequestDigest != goldenExecRequestDigest {
				t.Fatalf("digest = %s, want %s", canonical.RequestDigest, goldenExecRequestDigest)
			}
		})
	}
}

func TestDefaultPolicyVersionIsUnchanged(t *testing.T) {
	if got := DefaultPolicy().Version; got != "builtin-tools-v1" {
		t.Fatalf("policy version = %q, want builtin-tools-v1", got)
	}
}
