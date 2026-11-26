package handoff

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
)

type recordingOpener struct {
	requests []Request
	err      error
}

func (o *recordingOpener) OpenHandoff(_ context.Context, request *Request) error {
	o.requests = append(o.requests, *request)
	return o.err
}

func handoffToolRequest(class principal.Class, args string) *toolbroker.ToolRequest {
	return &toolbroker.ToolRequest{
		SessionID:      "lobby/discord:owner/dm:601",
		PrincipalID:    "discord:601",
		PrincipalClass: class,
		AgentID:        "lobby",
		ToolName:       tools.HandoffToolName,
		ToolVersion:    tools.Version,
		Arguments:      json.RawMessage(args),
	}
}

func TestHandoffToolMatchesBuiltinCatalog(t *testing.T) {
	wantPtr, ok := tools.DefinitionsByKey()[tools.HandoffToolName+"@"+tools.Version]
	if !ok {
		t.Fatal("handoff_to_owner@v1 missing from builtin catalog")
	}
	got := ToolDefinition()
	if got.Validator == nil {
		t.Fatal("ToolDefinition().Validator must not be nil")
	}
	gotCopy := got
	gotCopy.Validator = nil
	wantCopy := *wantPtr
	wantCopy.Validator = nil
	if !reflect.DeepEqual(gotCopy, wantCopy) {
		t.Fatalf("ToolDefinition() non-func fields differ from catalog:\n got: %+v\nwant: %+v", gotCopy, wantCopy)
	}

	payloads := []struct {
		name string
		raw  string
		ok   bool
	}{
		{name: "valid", raw: `{"reason":"refund"}`, ok: true},
		{name: "missing reason", raw: `{}`, ok: false},
		{name: "empty reason", raw: `{"reason":""}`, ok: false},
		{name: "non-string reason", raw: `{"reason":123}`, ok: false},
		{name: "unknown key", raw: `{"reason":"x","extra":1}`, ok: false},
		{name: "trailing data", raw: `{"reason":"x"} {}`, ok: false},
		{name: "not object", raw: `["x"]`, ok: false},
		{name: "invalid json", raw: `{`, ok: false},
	}
	for _, tc := range payloads {
		t.Run(tc.name, func(t *testing.T) {
			gotOut, gotErr := got.Validate(json.RawMessage(tc.raw))
			wantOut, wantErr := wantPtr.Validate(json.RawMessage(tc.raw))
			if (gotErr == nil) != tc.ok {
				t.Fatalf("got.Validate(%s) err = %v, want ok=%t", tc.raw, gotErr, tc.ok)
			}
			if (gotErr == nil) != (wantErr == nil) {
				t.Fatalf("parity mismatch for %s: gotErr=%v wantErr=%v", tc.raw, gotErr, wantErr)
			}
			if tc.ok && !bytes.Equal(gotOut, wantOut) {
				t.Fatalf("validated output mismatch: got %s, want %s", gotOut, wantOut)
			}
		})
	}

	wantRule, ok := toolbroker.DefaultPolicy().Rules[tools.HandoffToolName]
	if !ok {
		t.Fatal("handoff_to_owner missing from DefaultPolicy")
	}
	if gotRule := ToolRule(); !reflect.DeepEqual(gotRule, wantRule) {
		t.Fatalf("ToolRule() = %+v, want %+v", gotRule, wantRule)
	}
}

func TestHandoffAdapterOpensForNonOwners(t *testing.T) {
	nilAdapter := NewToolProvider(nil).Tools()[0].Adapter
	if _, err := nilAdapter(t.Context(), handoffToolRequest(principal.ClassPublic, `{"reason":"refund"}`), approval.Constraints{}); !fault.HasCode(err, toolbroker.ResultCapabilityUnavailable) {
		t.Fatalf("handoff without an opener err = %v, want %s", err, toolbroker.ResultCapabilityUnavailable)
	}
	opener := &recordingOpener{}
	adapter := NewToolProvider(opener).Tools()[0].Adapter
	result, err := adapter(t.Context(), handoffToolRequest(principal.ClassPublic, `{"reason":"  refund for order 12  "}`), approval.Constraints{})
	if err != nil || result.Class != toolbroker.ResultOK {
		t.Fatalf("adapter = %+v, %v", result, err)
	}
	if len(opener.requests) != 1 || opener.requests[0] != (Request{SessionID: "lobby/discord:owner/dm:601", PrincipalID: "discord:601", AgentID: "lobby", Reason: "refund for order 12"}) {
		t.Fatalf("opened = %+v", opener.requests)
	}
	var output map[string]string
	if err := json.Unmarshal(result.Output, &output); err != nil || output["status"] != "handed_off" {
		t.Fatalf("output = %s, %v", result.Output, err)
	}
	for _, class := range []principal.Class{principal.ClassOwner, ""} {
		if _, err := adapter(t.Context(), handoffToolRequest(class, `{"reason":"x"}`), approval.Constraints{}); !fault.HasCode(err, toolbroker.ResultPolicyDenied) {
			t.Fatalf("class %q err = %v, want %s", class, err, toolbroker.ResultPolicyDenied)
		}
	}
	opener.err = errors.New("dm closed")
	if _, err := adapter(t.Context(), handoffToolRequest(principal.ClassTrusted, `{"reason":"x"}`), approval.Constraints{}); !fault.HasCode(err, toolbroker.ResultCapabilityUnavailable) {
		t.Fatalf("failed notification err = %v, want %s", err, toolbroker.ResultCapabilityUnavailable)
	}
}

func TestHandoffDefinitionIsReadTierAndStrict(t *testing.T) {
	definition := ToolDefinition()
	if definition.Effectful || definition.RequiresApproval || !definition.OptIn || tools.EffectTier(tools.HandoffToolName) {
		t.Fatalf("definition = %+v, want read tier and opt-in", definition)
	}
	for _, raw := range []string{`{}`, `{"reason":""}`, `{"reason":"x","extra":1}`} {
		if _, err := definition.Validate(json.RawMessage(raw)); err == nil {
			t.Errorf("Validate(%s) accepted", raw)
		}
	}
	if _, err := definition.Validate(json.RawMessage(`{"reason":"refund"}`)); err != nil {
		t.Fatalf("Validate(valid): %v", err)
	}
}
