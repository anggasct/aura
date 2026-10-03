package runtimeengine

import (
	"encoding/json"
	"testing"

	"github.com/anggasct/aura/internal/runtime"
	runtimeingress "github.com/anggasct/aura/internal/runtime/ingress"
)

func TestImagePartsSurviveDescriptorRoundTrip(t *testing.T) {
	envelope := json.RawMessage(`{"kind":"image_ref.v1"}`)
	req := &runtime.TurnRequest{
		TurnID:      "turn-image",
		SessionID:   "sess-1",
		PrincipalID: "owner",
		Origin:      runtime.OriginTerminal,
		Parts: []runtimeingress.InputPart{
			{Text: "before"},
			{Image: envelope},
			{Text: "after"},
		},
	}
	desc := turnDescriptorFromRequest(req)
	if len(desc.Parts) != 3 || string(desc.Parts[1].Image) != string(envelope) {
		t.Fatalf("descriptor parts = %+v", desc.Parts)
	}
	restored := turnRequestFromDescriptor(&desc)
	if len(restored.Parts) != 3 || string(restored.Parts[1].Image) != string(envelope) {
		t.Fatalf("restored parts = %+v", restored.Parts)
	}
	if restored.Parts[0].Text != "before" || restored.Parts[2].Text != "after" {
		t.Fatalf("order broken: %+v", restored.Parts)
	}
}
