package toolsbuiltin

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/toolbroker"
	"github.com/anggasct/aura/internal/tools"
)

type recordingOpener struct {
	requests []HandoffRequest
	err      error
}

func (o *recordingOpener) OpenHandoff(_ context.Context, request *HandoffRequest) error {
	o.requests = append(o.requests, *request)
	return o.err
}

func handoffRequest(class principal.Class, args string) *toolbroker.ToolRequest {
	return &toolbroker.ToolRequest{
		SessionID: "lobby/discord:owner/dm:601", PrincipalID: "discord:601", PrincipalClass: class, AgentID: "lobby",
		ToolName: tools.HandoffToolName, ToolVersion: tools.Version, Arguments: json.RawMessage(args),
	}
}

func TestHandoffAdapterOpensForNonOwners(t *testing.T) {
	holder := &handoffHolder{}
	if _, err := holder.adapter(t.Context(), handoffRequest(principal.ClassPublic, `{"reason":"refund"}`), approval.Constraints{}); err == nil {
		t.Fatal("handoff without an opener succeeded")
	}
	opener := &recordingOpener{}
	executor := &Executor{handoff: holder}
	executor.SetHandoffOpener(opener)
	result, err := holder.adapter(t.Context(), handoffRequest(principal.ClassPublic, `{"reason":"  refund for order 12  "}`), approval.Constraints{})
	if err != nil || result.Class != toolbroker.ResultOK {
		t.Fatalf("adapter = %+v, %v", result, err)
	}
	if len(opener.requests) != 1 || opener.requests[0] != (HandoffRequest{SessionID: "lobby/discord:owner/dm:601", PrincipalID: "discord:601", AgentID: "lobby", Reason: "refund for order 12"}) {
		t.Fatalf("opened = %+v", opener.requests)
	}
	var output map[string]string
	if err := json.Unmarshal(result.Output, &output); err != nil || output["status"] != "handed_off" {
		t.Fatalf("output = %s, %v", result.Output, err)
	}
	for _, class := range []principal.Class{principal.ClassOwner, ""} {
		if _, err := holder.adapter(t.Context(), handoffRequest(class, `{"reason":"x"}`), approval.Constraints{}); err == nil {
			t.Fatalf("class %q handed off", class)
		}
	}
	opener.err = errors.New("dm closed")
	if _, err := holder.adapter(t.Context(), handoffRequest(principal.ClassTrusted, `{"reason":"x"}`), approval.Constraints{}); err == nil {
		t.Fatal("failed notification reported success")
	}
}

func TestHandoffDefinitionIsReadTierAndStrict(t *testing.T) {
	definition := tools.DefinitionsByKey()[tools.HandoffToolName+"@"+tools.Version]
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
