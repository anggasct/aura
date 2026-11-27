package toolbroker

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/tools"
)

func unusedAdapter(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
	return ToolResult{}, nil
}

func TestBrokerRejectsInconsistentOperationDefinitions(t *testing.T) {
	registration := func(stub operationStub, mutate func(registration *ToolRegistration)) ToolRegistration {
		definition := stub.definition(t)
		built := ToolRegistration{Definition: definition, Adapter: unusedAdapter, Rule: DefaultRule(&definition)}
		if needsPreview(&definition) {
			built.Preview = defaultPreview
		}
		if mutate != nil {
			mutate(&built)
		}
		return built
	}
	cases := []struct {
		name  string
		build ToolRegistration
		coded bool
	}{
		{name: "a read with fresh approval", build: registration(readStub, func(r *ToolRegistration) { r.Definition.FreshApproval = true }), coded: true},
		{name: "an operation without a risk", build: registration(changeStub, func(r *ToolRegistration) { r.Definition.Risk = "" }), coded: true},
		{name: "an operation with an unknown risk", build: registration(changeStub, func(r *ToolRegistration) { r.Definition.Risk = "sometimes" }), coded: true},
		{name: "a risk without an operation", build: registration(changeStub, func(r *ToolRegistration) { r.Definition.Operation = "" }), coded: true},
		{name: "an owner notice without an operation", build: registration(readStub, func(r *ToolRegistration) {
			r.Definition.Operation, r.Definition.Risk, r.Definition.OwnerNotice = "", "", true
		}), coded: true},
		{name: "fresh approval without an operation", build: registration(readStub, func(r *ToolRegistration) {
			r.Definition.Operation, r.Definition.Risk, r.Definition.FreshApproval = "", "", true
		}), coded: true},
		{name: "an effectful flag that does not match the risk", build: registration(readStub, func(r *ToolRegistration) { r.Definition.Effectful = true }), coded: true},
		{name: "a trust grant that skips approval", build: registration(trustGrantStub, func(r *ToolRegistration) { r.Definition.RequiresApproval = false }), coded: true},
		{name: "a destructive operation that allows standing grants", build: registration(destructiveStub, func(r *ToolRegistration) { r.Definition.NoStandingGrant = false }), coded: true},
		{name: "a change without exact display", build: registration(changeStub, func(r *ToolRegistration) { r.Definition.ExactDisplay = false }), coded: true},
		{name: "a fresh change that does not require approval", build: registration(freshStub, func(r *ToolRegistration) { r.Definition.RequiresApproval = false }), coded: true},
		{name: "an operation the generic loop could offer", build: registration(readStub, func(r *ToolRegistration) { r.Definition.OptIn = false }), coded: true},
		{name: "an operation offered to every owner persona", build: registration(readStub, func(r *ToolRegistration) { r.Definition.OwnerOptIn = false }), coded: true},
		{name: "an orchestration operation", build: registration(readStub, func(r *ToolRegistration) { r.Definition.Orchestration = true }), coded: true},
		{name: "a plugin operation", build: registration(readStub, func(r *ToolRegistration) { r.Definition.Plugin = &tools.PluginRef{ID: "p"} }), coded: true},
		{name: "an owner notice that is a sink", build: registration(noticeStub, func(r *ToolRegistration) { r.Definition.ExternalSink = true }), coded: true},
		{name: "an owner notice that is a trust grant", build: registration(trustGrantStub, func(r *ToolRegistration) { r.Definition.OwnerNotice = true }), coded: true},
		{name: "a rule that does not require approval for a privileged operation", build: registration(destructiveStub, func(r *ToolRegistration) { r.Rule.RequiresApproval = false })},
		{name: "a trust grant without a previewer", build: registration(trustGrantStub, func(r *ToolRegistration) { r.Preview = nil })},
		{name: "a destructive operation without a previewer", build: registration(destructiveStub, func(r *ToolRegistration) { r.Preview = nil })},
		{name: "a read with a previewer", build: registration(readStub, func(r *ToolRegistration) { r.Preview = defaultPreview })},
		{name: "a legacy tool with a previewer", build: ToolRegistration{
			Definition: tools.Definition{Name: "legacy", Version: tools.Version, Validator: func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil }},
			Adapter:    unusedAdapter, Rule: approval.Rule{ToolName: "legacy", ToolVersion: tools.Version}, Preview: defaultPreview,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			broker := newProbeBroker(t)
			err := broker.RegisterProvider(stubOperations{tc.build})
			if err == nil {
				t.Fatal("RegisterProvider accepted the definition")
			}
			if tc.coded && !fault.HasCode(err, tools.ErrorCodeDefinitionInvalid) {
				t.Errorf("RegisterProvider = %v, want %s", err, tools.ErrorCodeDefinitionInvalid)
			}
			if !tc.coded && !fault.HasCode(err, ResultInvalidArgument) {
				t.Errorf("RegisterProvider = %v, want %s", err, ResultInvalidArgument)
			}
			if _, found := broker.Definition(tc.build.Definition.Name, tools.Version); found {
				t.Error("a rejected definition was registered")
			}
		})
	}
}

func TestRejectedOperationRegistrationRegistersNothing(t *testing.T) {
	broker := newProbeBroker(t)
	good := changeStub.definition(t)
	bad := readStub.definition(t)
	bad.FreshApproval = true
	err := broker.RegisterProvider(stubOperations{
		{Definition: good, Adapter: unusedAdapter, Rule: DefaultRule(&good)},
		{Definition: bad, Adapter: unusedAdapter, Rule: DefaultRule(&bad)},
	})
	if err == nil {
		t.Fatal("RegisterProvider accepted a provider with a defective definition")
	}
	if got := len(broker.Definitions()); got != 0 {
		t.Errorf("registered definitions = %d, want none after a rejected provider", got)
	}
}

func TestOperationToolWithoutAPreviewerOnATrustGrantCannotBeRegisteredOneByOne(t *testing.T) {
	broker := newProbeBroker(t)
	definition := trustGrantStub.definition(t)
	rule := DefaultRule(&definition)
	if err := broker.RegisterTool(&definition, unusedAdapter, &rule); !fault.HasCode(err, ResultInvalidArgument) {
		t.Fatalf("RegisterTool = %v, want %s", err, ResultInvalidArgument)
	}
}

func TestReplacingAnOperationToolDropsItsPreviewer(t *testing.T) {
	rig := newOperationRig(t, openOperationDatabase(t), &operationOptions{reachable: true}, operationStub{name: "aura_stub_swap", risk: tools.RiskChange, previewed: true})
	definition := operationStub{name: "aura_stub_swap", risk: tools.RiskChange}.definition(t)
	rule := DefaultRule(&definition)
	if err := rig.broker.RegisterTool(&definition, rig.handle, &rule); err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}
	if got := rig.execute(t, operationRequest("aura_stub_swap", "x")); got.class != ResultOK {
		t.Fatalf("outcome = %+v, want ok", got)
	}
	if previews := rig.previewed.Load(); previews != 0 {
		t.Errorf("previewer runs = %d, want none after the previewer was replaced away", previews)
	}
}

func TestOperationHelpersRefuseNilArguments(t *testing.T) {
	broker := newProbeBroker(t)
	definition := changeStub.definition(t)
	request := operationRequest(changeStub.name, "x")
	decision := approval.PolicyDecision{Outcome: approval.OutcomeAllow}
	rule := DefaultRule(&definition)
	cases := []struct {
		name string
		call func() error
	}{
		{name: "owner only without a request", call: func() error { return operationOwnerOnly(nil, "owner", &definition) }},
		{name: "owner only without a definition", call: func() error { return operationOwnerOnly(request, "owner", nil) }},
		{name: "refusal without a request", call: func() error { return operationRefusal(nil, &definition) }},
		{name: "refusal without a definition", call: func() error { return operationRefusal(request, nil) }},
		{name: "approval without a decision", call: func() error { return operationApproval(nil, request, &definition) }},
		{name: "approval without a request", call: func() error { return operationApproval(&decision, nil, &definition) }},
		{name: "approval without a definition", call: func() error { return operationApproval(&decision, request, nil) }},
		{name: "no approver without a request", call: func() error { return operationNoApprover(nil, &definition) }},
		{name: "no approver without a definition", call: func() error { return operationNoApprover(request, nil) }},
		{name: "registration without a definition", call: func() error { return checkOperationRegistration(nil, &rule, nil) }},
		{name: "registration without a rule", call: func() error { return checkOperationRegistration(&definition, nil, nil) }},
		{name: "admission without a call", call: func() error { _, err := broker.admitOperation(t.Context(), nil, nil); return err }},
		{name: "admission without a request", call: func() error {
			_, err := broker.admitOperation(t.Context(), nil, &operationCall{definition: &definition})
			return err
		}},
		{name: "preview without a previewer", call: func() error {
			_, err := broker.runPreview(t.Context(), nil, &operationCall{canonical: request, definition: &definition})
			return err
		}},
		{name: "preview action without a call", call: func() error { _, err := broker.previewAction(t.Context(), nil); return err }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !fault.HasCode(err, ResultInvalidArgument) {
				t.Fatalf("error = %v, want %s", err, ResultInvalidArgument)
			}
		})
	}
	if !unattendedRequest(nil, &definition) || !unattendedRequest(request, nil) {
		t.Error("a missing request or definition is not treated as unattended")
	}
	if !needsPreview(nil) {
		t.Error("a missing definition does not need a preview")
	}
	if providerFor(nil) != operationProvider {
		t.Errorf("a missing definition uses %s, want %s", providerFor(nil), operationProvider)
	}
}
