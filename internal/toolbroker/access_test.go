package toolbroker

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/capability/capabilitytest"
	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/tools"
)

type fakeAccess struct {
	ceilings  map[principal.Class][]string
	standing  bool
	lookupErr error
	lookups   int
	servers   []string
}

func (f *fakeAccess) ToolCeiling(agentID string, class principal.Class) ([]string, bool) {
	if agentID != "team" {
		return nil, false
	}
	return slices.Clone(f.ceilings[class]), true
}

func (f *fakeAccess) StandingGrant(_ context.Context, principalID, agentID, toolName, server string) (bool, error) {
	f.lookups++
	f.servers = append(f.servers, server)
	if f.lookupErr != nil {
		return false, f.lookupErr
	}
	return f.standing && principalID == "discord:111" && agentID == "team", nil
}

type accessBroker struct {
	broker   *Broker
	calls    map[string]int
	prompts  int
	approved bool
	states   []string
}

func newAccessBroker(t *testing.T, access AccessPolicy) *accessBroker {
	t.Helper()
	harness := &accessBroker{calls: map[string]int{}, approved: true}
	adapter := func(name string) Adapter {
		return func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
			harness.calls[name]++
			return ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
		}
	}
	broker, err := New(&Options{
		HostCapabilities: capabilitytest.Host(t), Effects: newBrokerEffectExecutor(t),
		Adapters: map[string]Adapter{
			"exec@v1":      adapter("exec"),
			"read_file@v1": adapter("read_file"),
			"web_fetch@v1": adapter("web_fetch"),
		},
		ApprovalDecider: func(context.Context, *ApprovalPrompt) (bool, error) {
			harness.prompts++
			return harness.approved, nil
		},
		Observer: func(_ context.Context, observation Observation) {
			harness.states = append(harness.states, observation.Approval)
		},
		Access: access,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	harness.broker = broker
	return harness
}

func classifiedRequest(name, arguments string, class principal.Class, capabilities ...string) *ToolRequest {
	request := brokerRequest(name, arguments, capabilities...)
	request.PrincipalID = "discord:111"
	request.PrincipalClass = class
	request.AgentID = "team"
	request.EventSequence = 1
	return request
}

func execRequest(class principal.Class) *ToolRequest {
	return classifiedRequest("exec", `{"command":["true"]}`, class, "exec-linux")
}

func readRequest(class principal.Class) *ToolRequest {
	return classifiedRequest("read_file", `{"path":"notes.txt"}`, class, "workspace-read")
}

func teamAccess() *fakeAccess {
	return &fakeAccess{ceilings: map[principal.Class][]string{
		principal.ClassTrusted: {"exec", "read_file"},
		principal.ClassPublic:  {"read_file"},
	}}
}

func TestPublicPrincipalCannotRunEffectTools(t *testing.T) {
	access := teamAccess()
	access.ceilings[principal.ClassPublic] = []string{"read_file", "exec"}
	harness := newAccessBroker(t, access)
	_, err := harness.broker.Execute(t.Context(), execRequest(principal.ClassPublic))
	if classOf(err) != ResultToolTierDenied {
		t.Fatalf("public exec error = %v, want tool_tier_denied", err)
	}
	if harness.calls["exec"] != 0 || harness.prompts != 0 {
		t.Fatalf("exec calls = %d prompts = %d, want neither", harness.calls["exec"], harness.prompts)
	}
	if _, err := harness.broker.Execute(t.Context(), readRequest(principal.ClassPublic)); err != nil {
		t.Fatalf("public read_file: %v", err)
	}
	if harness.calls["read_file"] != 1 {
		t.Fatalf("read_file calls = %d, want 1", harness.calls["read_file"])
	}
}

func TestNonOwnerToolsOutsideTheCeilingAreDenied(t *testing.T) {
	cases := map[string]struct {
		access AccessPolicy
		req    *ToolRequest
	}{
		"public tool not in public_tools":     {teamAccess(), classifiedRequest("web_fetch", `{"url":"https://example.com"}`, principal.ClassPublic, "public-web")},
		"trusted tool not in principal_tools": {teamAccess(), classifiedRequest("web_fetch", `{"url":"https://example.com"}`, principal.ClassTrusted, "public-web")},
		"unknown persona":                     {teamAccess(), func() *ToolRequest { r := readRequest(principal.ClassTrusted); r.AgentID = "other"; return r }()},
		"no access policy":                    {nil, readRequest(principal.ClassTrusted)},
		"unrecognized class":                  {teamAccess(), func() *ToolRequest { r := readRequest("guest"); r.AgentID = "other"; return r }()},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			harness := newAccessBroker(t, tc.access)
			_, err := harness.broker.Execute(t.Context(), tc.req)
			if classOf(err) != ResultToolTierDenied {
				t.Fatalf("Execute error = %v, want tool_tier_denied", err)
			}
			if len(harness.calls) != 0 {
				t.Fatalf("adapter calls = %v, want none", harness.calls)
			}
		})
	}
}

func TestTrustedEffectCallNeedsApprovalUnlessAStandingGrantCoversIt(t *testing.T) {
	access := teamAccess()
	harness := newAccessBroker(t, access)
	if _, err := harness.broker.Execute(t.Context(), execRequest(principal.ClassTrusted)); err != nil {
		t.Fatalf("approved trusted exec: %v", err)
	}
	if harness.prompts != 1 || harness.calls["exec"] != 1 || harness.states[0] != ApprovalApproved {
		t.Fatalf("prompts = %d calls = %d states = %v, want one approved card", harness.prompts, harness.calls["exec"], harness.states)
	}
	access.standing = true
	second := execRequest(principal.ClassTrusted)
	second.RequestID, second.IdempotencyKey, second.EventSequence = "request-2", "idempotency-2", 2
	if _, err := harness.broker.Execute(t.Context(), second); err != nil {
		t.Fatalf("standing trusted exec: %v", err)
	}
	if harness.prompts != 1 || harness.calls["exec"] != 2 || harness.states[1] != ApprovalStanding {
		t.Fatalf("prompts = %d calls = %d states = %v, want the standing grant to skip the card", harness.prompts, harness.calls["exec"], harness.states)
	}
	other := execRequest(principal.ClassTrusted)
	other.PrincipalID = "discord:222"
	other.RequestID, other.IdempotencyKey, other.EventSequence = "request-3", "idempotency-3", 3
	if _, err := harness.broker.Execute(t.Context(), other); err != nil {
		t.Fatalf("other principal exec: %v", err)
	}
	if harness.prompts != 2 {
		t.Fatalf("prompts = %d, want another principal to still need a card", harness.prompts)
	}
	harness.approved = false
	rejected := execRequest(principal.ClassTrusted)
	rejected.PrincipalID = "discord:222"
	rejected.RequestID, rejected.IdempotencyKey, rejected.EventSequence = "request-4", "idempotency-4", 4
	if _, err := harness.broker.Execute(t.Context(), rejected); classOf(err) != ResultApprovalRequired {
		t.Fatalf("rejected exec error = %v, want approval_required", err)
	}
}

func TestPublicPrincipalsNeverConsultStandingGrants(t *testing.T) {
	access := teamAccess()
	access.standing = true
	harness := newAccessBroker(t, access)
	_, _ = harness.broker.Execute(t.Context(), execRequest(principal.ClassPublic))
	_, _ = harness.broker.Execute(t.Context(), readRequest(principal.ClassPublic))
	if access.lookups != 0 {
		t.Fatalf("standing grant lookups = %d, want none for public principals", access.lookups)
	}
}

func TestStandingGrantLookupFailureFailsClosed(t *testing.T) {
	access := teamAccess()
	access.lookupErr = errors.New("storage busy")
	harness := newAccessBroker(t, access)
	_, err := harness.broker.Execute(t.Context(), execRequest(principal.ClassTrusted))
	if classOf(err) != ResultPolicyDenied {
		t.Fatalf("Execute error = %v, want policy_denied", err)
	}
	if harness.calls["exec"] != 0 || harness.prompts != 0 {
		t.Fatalf("calls = %d prompts = %d, want neither after a failed lookup", harness.calls["exec"], harness.prompts)
	}
}

func TestTrustedEffectToolWithoutApprovalRuleStillNeedsTheOwner(t *testing.T) {
	access := teamAccess()
	access.ceilings[principal.ClassTrusted] = append(access.ceilings[principal.ClassTrusted], "journal_note")
	harness := newAccessBroker(t, access)
	definition := tools.Definition{
		Name: "journal_note", Version: "v1", Effectful: true,
		Validator: func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil },
	}
	rule := approval.Rule{
		ToolName: "journal_note", ToolVersion: "v1",
		AllowedTrust: []approval.TrustLabel{approval.TrustOwnerInput},
		Constraints:  approval.Constraints{MaxOutputBytes: 1024, Timeout: 10 * time.Second},
	}
	if err := harness.broker.RegisterTool(&definition, func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
		harness.calls["journal_note"]++
		return ToolResult{Output: json.RawMessage(`{}`)}, nil
	}, &rule); err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}
	owner := classifiedRequest("journal_note", `{"text":"hi"}`, principal.ClassOwner)
	if _, err := harness.broker.Execute(t.Context(), owner); err != nil {
		t.Fatalf("owner journal_note: %v", err)
	}
	if harness.prompts != 0 {
		t.Fatalf("owner prompts = %d, want the rule to stay auto-approved", harness.prompts)
	}
	trusted := classifiedRequest("journal_note", `{"text":"hi"}`, principal.ClassTrusted)
	trusted.RequestID, trusted.IdempotencyKey, trusted.EventSequence = "request-2", "idempotency-2", 2
	if _, err := harness.broker.Execute(t.Context(), trusted); err != nil {
		t.Fatalf("trusted journal_note: %v", err)
	}
	if harness.prompts != 1 || harness.calls["journal_note"] != 2 {
		t.Fatalf("prompts = %d calls = %d, want one owner card for the trusted call", harness.prompts, harness.calls["journal_note"])
	}
}

func TestUnclassifiedRequestsKeepTheShippedPolicy(t *testing.T) {
	harness := newAccessBroker(t, teamAccess())
	request := brokerRequest("read_file", `{"path":"notes.txt"}`, "workspace-read")
	if _, err := harness.broker.Execute(t.Context(), request); err != nil {
		t.Fatalf("unclassified read_file: %v", err)
	}
}

func registerServerTool(t *testing.T, harness *accessBroker, name string) {
	t.Helper()
	definition := &tools.Definition{Name: name, Version: "v1", Server: "docs", Schema: json.RawMessage(`{"type":"object"}`), RequiresApproval: true, Effectful: true, Validator: func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil }}
	rule := approval.Rule{
		ToolName: name, ToolVersion: "v1", RequiresApproval: true,
		AllowedTrust: []approval.TrustLabel{approval.TrustOwnerInput, approval.TrustDerivedUntrusted},
	}
	adapter := func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
		harness.calls[name]++
		return ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
	}
	if err := harness.broker.RegisterTool(definition, adapter, &rule); err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}
}

func TestPublicPrincipalsNeverRunServerToolsEvenInsideTheCeiling(t *testing.T) {
	access := teamAccess()
	access.ceilings[principal.ClassPublic] = append(access.ceilings[principal.ClassPublic], "mcp_docs", "mcp_docs_search")
	access.standing = true
	harness := newAccessBroker(t, access)
	registerServerTool(t, harness, "mcp_docs_search")
	_, err := harness.broker.Execute(t.Context(), classifiedRequest("mcp_docs_search", `{}`, principal.ClassPublic))
	if classOf(err) != ResultToolTierDenied {
		t.Fatalf("public error = %v, want tool_tier_denied", err)
	}
	if harness.calls["mcp_docs_search"] != 0 || harness.prompts != 0 || access.lookups != 0 {
		t.Fatalf("calls = %d prompts = %d lookups = %d, want none", harness.calls["mcp_docs_search"], harness.prompts, access.lookups)
	}
}

func TestTrustedServerToolsFollowThePrincipalCeiling(t *testing.T) {
	cases := map[string]struct {
		ceiling []string
		allowed bool
	}{
		"exact tool entry":                {[]string{"mcp_docs_search"}, true},
		"server entry covers its tools":   {[]string{"mcp_docs"}, true},
		"sibling tool entry":              {[]string{"mcp_docs_read"}, false},
		"other server entry":              {[]string{"mcp_files"}, false},
		"longer tool entry":               {[]string{"mcp_docs_search_all"}, false},
		"built-in tools only":             {[]string{"exec", "read_file"}, false},
		"server entry of a longer server": {[]string{"mcp_docs_extra"}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			access := teamAccess()
			access.ceilings[principal.ClassTrusted] = tc.ceiling
			harness := newAccessBroker(t, access)
			registerServerTool(t, harness, "mcp_docs_search")
			_, err := harness.broker.Execute(t.Context(), classifiedRequest("mcp_docs_search", `{}`, principal.ClassTrusted))
			if tc.allowed {
				if err != nil {
					t.Fatalf("Execute: %v", err)
				}
				return
			}
			if classOf(err) != ResultToolTierDenied {
				t.Fatalf("Execute error = %v, want tool_tier_denied", err)
			}
			if harness.calls["mcp_docs_search"] != 0 || harness.prompts != 0 {
				t.Fatalf("calls = %d prompts = %d, want neither", harness.calls["mcp_docs_search"], harness.prompts)
			}
		})
	}
}

func TestTrustedServerToolCallsGoToTheOwnerForApproval(t *testing.T) {
	access := teamAccess()
	access.ceilings[principal.ClassTrusted] = []string{"mcp_docs"}
	harness := newAccessBroker(t, access)
	registerServerTool(t, harness, "mcp_docs_search")
	if _, err := harness.broker.Execute(t.Context(), classifiedRequest("mcp_docs_search", `{}`, principal.ClassTrusted)); err != nil {
		t.Fatalf("approved trusted call: %v", err)
	}
	if harness.prompts != 1 || harness.calls["mcp_docs_search"] != 1 || harness.states[0] != ApprovalApproved {
		t.Fatalf("prompts = %d calls = %d states = %v, want one approved owner card", harness.prompts, harness.calls["mcp_docs_search"], harness.states)
	}
	harness.approved = false
	rejected := classifiedRequest("mcp_docs_search", `{}`, principal.ClassTrusted)
	rejected.RequestID, rejected.IdempotencyKey, rejected.EventSequence = "request-2", "idempotency-2", 2
	if _, err := harness.broker.Execute(t.Context(), rejected); classOf(err) != ResultApprovalRequired {
		t.Fatalf("rejected call error = %v, want approval_required", err)
	}
	if harness.calls["mcp_docs_search"] != 1 {
		t.Fatalf("calls = %d, want the rejected call not to run", harness.calls["mcp_docs_search"])
	}
	harness.approved = true
	access.standing = true
	standing := classifiedRequest("mcp_docs_search", `{}`, principal.ClassTrusted)
	standing.RequestID, standing.IdempotencyKey, standing.EventSequence = "request-3", "idempotency-3", 3
	if _, err := harness.broker.Execute(t.Context(), standing); err != nil {
		t.Fatalf("standing trusted call: %v", err)
	}
	if harness.prompts != 2 || harness.calls["mcp_docs_search"] != 2 || !slices.Equal(slices.Compact(access.servers), []string{"docs"}) {
		t.Fatalf("prompts = %d calls = %d lookup servers = %v, want the grant lookup to name the server and skip the card", harness.prompts, harness.calls["mcp_docs_search"], access.servers)
	}
}
