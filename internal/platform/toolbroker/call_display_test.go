package toolbroker

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/principal"
)

func TestCallDisplayRendersTheTextOfTheApprovalCard(t *testing.T) {
	arguments := `{"to":"a@mail.example","body":"token sk-canary-9f2a end"}`
	h := newDisplayHarness(t, []string{"to", "body"}, "sk-canary-9f2a")
	if err := h.run(arguments, principal.ClassTrusted); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	card := h.promptList()[0].Arguments
	text, err := h.broker.CallDisplay(displayToolName, json.RawMessage(arguments))
	if err != nil || text != card {
		t.Fatalf("CallDisplay = %q, %v, want the card text %q", text, err, card)
	}
	if strings.Contains(text, "sk-canary-9f2a") {
		t.Fatalf("CallDisplay leaked a configured secret: %q", text)
	}
}

func TestCallDisplayRefusesWhatTheCardWouldRefuse(t *testing.T) {
	cases := []struct {
		name      string
		tool      string
		arguments string
		want      ResultClass
	}{
		{name: "a call that does not fit", tool: displayToolName, arguments: `{"body":"` + strings.Repeat("b", 2000) + `","to":"a@mail.example"}`, want: ResultApprovalDisplayTooLarge},
		{name: "duplicate keys", tool: displayToolName, arguments: `{"to":"a@mail.example","to":"attacker@evil.example"}`, want: ResultInvalidArgument},
		{name: "an unknown tool", tool: "ghost_tool", arguments: `{}`, want: ResultPolicyDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newDisplayHarness(t, []string{"body", "to"})
			text, err := h.broker.CallDisplay(tc.tool, json.RawMessage(tc.arguments))
			if classOf(err) != tc.want || text != "" {
				t.Fatalf("CallDisplay = %q, %v, want no text and %s", text, err, tc.want)
			}
		})
	}
}
