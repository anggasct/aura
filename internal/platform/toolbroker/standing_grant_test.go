package toolbroker

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/tools"
)

const probeToolName = "probe_effect"

type probeRig struct {
	broker  *Broker
	access  *fakeAccess
	prompts []*ApprovalPrompt
}

func newProbeRig(t *testing.T, definition *tools.Definition, guard ApprovalGuard, standing bool, secrets ...string) *probeRig {
	t.Helper()
	rig := &probeRig{access: &fakeAccess{standing: standing, ceilings: map[principal.Class][]string{principal.ClassTrusted: {probeToolName}}}}
	options := &Options{
		HostCapabilities: capabilitytest.Host(t), Effects: newBrokerEffectExecutor(t),
		Access: rig.access, Secrets: secrets,
		ApprovalDecider: func(_ context.Context, prompt *ApprovalPrompt) (bool, error) {
			rig.prompts = append(rig.prompts, prompt)
			return true, nil
		},
	}
	if guard != nil {
		options.ApprovalGuards = map[string]ApprovalGuard{probeToolName: guard}
	}
	broker, err := New(options)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	definition.Name, definition.Version = probeToolName, "v1"
	definition.Schema = json.RawMessage(`{"type":"object"}`)
	definition.RequiresApproval, definition.Effectful = true, true
	definition.Validator = strictObjectValidator
	rule := approval.Rule{
		ToolName: probeToolName, ToolVersion: "v1", RequiresApproval: true,
		AllowedTrust: []approval.TrustLabel{approval.TrustOwnerInput},
	}
	adapter := func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
		return ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
	}
	if err := broker.RegisterTool(definition, adapter, &rule); err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}
	rig.broker = broker
	return rig
}

func (r *probeRig) run(t *testing.T) {
	t.Helper()
	if _, err := r.broker.Execute(t.Context(), classifiedRequest(probeToolName, `{"a":"b"}`, principal.ClassTrusted)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestNoStandingGrantToolsAlwaysAskTheOwner(t *testing.T) {
	cases := []struct {
		name        string
		definition  tools.Definition
		guard       ApprovalGuard
		standing    bool
		wantLookups int
		wantPrompts int
		wantAllowed bool
	}{
		{name: "flagged tool with a standing grant", definition: tools.Definition{NoStandingGrant: true}, standing: true, wantPrompts: 1},
		{name: "unflagged tool rides the standing grant", standing: true, wantLookups: 1},
		{name: "ordinary effectful tool", wantLookups: 1, wantPrompts: 1, wantAllowed: true},
		{name: "plugin effectful tool", definition: tools.Definition{Plugin: &tools.PluginRef{ID: "mailer"}}, wantPrompts: 1},
		{name: "guarded call with a standing grant", guard: guardReturning(true, nil), standing: true, wantPrompts: 1},
		{name: "unguarded call", guard: guardReturning(false, nil), wantLookups: 1, wantPrompts: 1, wantAllowed: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newProbeRig(t, &tc.definition, tc.guard, tc.standing)
			rig.run(t)
			if rig.access.lookups != tc.wantLookups || len(rig.prompts) != tc.wantPrompts {
				t.Fatalf("lookups = %d prompts = %d, want %d and %d", rig.access.lookups, len(rig.prompts), tc.wantLookups, tc.wantPrompts)
			}
			if len(rig.prompts) == 1 && rig.prompts[0].StandingGrantAllowed != tc.wantAllowed {
				t.Fatalf("StandingGrantAllowed = %t, want %t", rig.prompts[0].StandingGrantAllowed, tc.wantAllowed)
			}
		})
	}
}
