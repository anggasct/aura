package secret

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
)

func TestParseRefAcceptsEnvAndAbsoluteFileOnly(t *testing.T) {
	accepted := []struct {
		text string
		want Reference
	}{
		{"env://A_1", Reference{Env: "A_1"}},
		{"env://_lower9", Reference{Env: "_lower9"}},
		{"file:///run/x", Reference{File: "/run/x"}},
		{"", Reference{}},
	}
	for _, tc := range accepted {
		t.Run("accepts "+tc.text, func(t *testing.T) {
			ref, err := ParseRef(tc.text)
			if err != nil {
				t.Fatalf("ParseRef(%q): %v", tc.text, err)
			}
			if ref.String() != tc.text {
				t.Errorf("String() = %q, want the input unchanged", ref.String())
			}
			if ref.IsZero() != (tc.text == "") {
				t.Errorf("IsZero() = %v for %q", ref.IsZero(), tc.text)
			}
			if got := ref.Reference(); got != tc.want {
				t.Errorf("Reference() = %+v, want %+v", got, tc.want)
			}
		})
	}
	refused := []string{
		"env://1A", "env://", "env://a-b", "env://s05-canary-value",
		"file://rel", "file://s05-canary-value", "file:///a/../b", "file:///a/", "file://",
		"https://x", "https://s05-canary-value", "plain", "s05-canary-value", " env://A", "ENV://A",
	}
	for _, text := range refused {
		t.Run("refuses "+text, func(t *testing.T) {
			ref, err := ParseRef(text)
			if !fault.HasCode(err, ErrorCodeScopeViolation) {
				t.Fatalf("ParseRef(%q) = %v, want secret_scope_violation", text, err)
			}
			if !ref.IsZero() {
				t.Errorf("ParseRef(%q) returned %q with an error", text, ref.String())
			}
			if strings.Contains(err.Error(), "canary") {
				t.Errorf("error %q echoes the text after the scheme", err)
			}
		})
	}
	t.Run("round trips through json text", func(t *testing.T) {
		type holder struct {
			Required Ref `json:"required"`
			Optional Ref `json:"optional,omitzero"`
		}
		var decoded holder
		if err := json.Unmarshal([]byte(`{"required":"env://A_1","optional":"file:///run/x"}`), &decoded); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		encoded, err := json.Marshal(decoded)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if string(encoded) != `{"required":"env://A_1","optional":"file:///run/x"}` {
			t.Errorf("round trip = %s", encoded)
		}
		empty, err := json.Marshal(holder{})
		if err != nil {
			t.Fatalf("Marshal zero: %v", err)
		}
		if string(empty) != `{"required":""}` {
			t.Errorf("zero value = %s, want the required key empty and the optional one omitted", empty)
		}
		if err := json.Unmarshal([]byte(`{"required":"plain"}`), &decoded); err == nil {
			t.Error("Unmarshal accepted a reference without a scheme")
		}
	})
}
