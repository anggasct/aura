package runtimesessions

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPartImageEnvelopeRoundTrip(t *testing.T) {
	desc := testDescriptor("turn-image")
	desc.Parts = []Part{{Text: "look", Image: json.RawMessage(`{"kind":"image_ref.v1"}`)}}
	if err := desc.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	raw, err := json.Marshal(desc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded Descriptor
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(decoded.Parts[0].Image) != `{"kind":"image_ref.v1"}` {
		t.Fatalf("image envelope = %s", decoded.Parts[0].Image)
	}
}

func TestPartImageEnvelopeBounds(t *testing.T) {
	if err := (&Part{Image: json.RawMessage(`{invalid}`)}).Validate(); err == nil {
		t.Fatal("expected invalid json rejection")
	}
	huge := json.RawMessage(`"` + strings.Repeat("x", MaxImageEnvelopeBytes) + `"`)
	if err := (&Part{Image: huge}).Validate(); err == nil {
		t.Fatal("expected oversize envelope rejection")
	}
	if err := (&Part{Text: "plain"}).Validate(); err != nil {
		t.Fatalf("text-only part: %v", err)
	}
}
