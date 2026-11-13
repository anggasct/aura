package toolbroker

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/principal"
)

func TestExactCallTextRendersTheTextOfTheApprovalCard(t *testing.T) {
	arguments := `{"to":"a@mail.example","body":"short note"}`
	h := newDisplayHarness(t, []string{"to", "body"})
	if err := h.run(arguments, principal.ClassTrusted); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	card := h.promptList()[0].Arguments
	text, err := h.broker.ExactCallText(displayToolName, json.RawMessage(arguments))
	if err != nil || text != card {
		t.Fatalf("ExactCallText = %q, %v, want the card text %q", text, err, card)
	}
}

func TestExactCallTextRefusesWhatTheCardWouldCutOrRedact(t *testing.T) {
	cases := []struct {
		name      string
		tool      string
		arguments string
		secrets   []string
		want      ResultClass
	}{
		{name: "a call that does not fit", tool: displayToolName, arguments: `{"body":"` + strings.Repeat("b", 2000) + `","to":"a@mail.example"}`, want: ResultApprovalDisplayTooLarge},
		{name: "a call that holds a configured secret", tool: displayToolName, arguments: `{"to":"a@mail.example","body":"token sk-canary-9f2a end"}`, secrets: []string{"sk-canary-9f2a"}, want: ResultPolicyDenied},
		{name: "duplicate keys", tool: displayToolName, arguments: `{"to":"a@mail.example","to":"attacker@evil.example"}`, want: ResultInvalidArgument},
		{name: "an unknown tool", tool: "ghost_tool", arguments: `{}`, want: ResultPolicyDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newDisplayHarness(t, []string{"body", "to"}, tc.secrets...)
			text, err := h.broker.ExactCallText(tc.tool, json.RawMessage(tc.arguments))
			if classOf(err) != tc.want || text != "" {
				t.Fatalf("ExactCallText = %q, %v, want no text and %s", text, err, tc.want)
			}
		})
	}
}

func TestApprovalCardStillRedactsWhatExactCallTextRefuses(t *testing.T) {
	arguments := `{"to":"a@mail.example","body":"token sk-canary-9f2a end"}`
	h := newDisplayHarness(t, []string{"to", "body"}, "sk-canary-9f2a")
	if err := h.run(arguments, principal.ClassTrusted); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	card := h.promptList()[0].Arguments
	if strings.Contains(card, "sk-canary-9f2a") {
		t.Fatalf("card leaked a configured secret: %q", card)
	}
}

func TestApprovalCardStillCutsWhatExactCallTextRefuses(t *testing.T) {
	arguments := `{"to":"a@mail.example","body":"` + strings.Repeat("b", 2000) + `"}`
	h := newDisplayHarness(t, []string{"to", "body"})
	if err := h.run(arguments, principal.ClassTrusted); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if card := h.promptList()[0].Arguments; !strings.Contains(card, "bytes hidden") {
		t.Fatalf("card = %q, want the cut marked", card)
	}
	if text, err := h.broker.ExactCallText(displayToolName, json.RawMessage(arguments)); err == nil || text != "" {
		t.Fatalf("ExactCallText = %q, %v, want a refusal", text, err)
	}
}
