package runtimeadk

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
)

const wantTurnProtocol = `Turn rules:
- Before calling a tool, write one short status line of at most six words in the user's language that says what you are doing in plain words, without naming tools, arguments, or secrets.
- When you need a tool, call it in this same reply. Never end a reply by promising to do something later, such as "one moment, I will check"; either do it now or say what you could not do.
- If a tool returns the same error twice, do not call it again with the same approach: tell the user what you tried, why it failed, and what they can do next.
- Write every reply and status line in the language of the user's latest message.`

func TestTurnProtocolIsAppendedLast(t *testing.T) {
	cases := []struct {
		name        string
		instruction string
		want        string
	}{
		{name: "after the persona instructions", instruction: "  Be helpful.\n", want: "Be helpful.\n\n" + wantTurnProtocol},
		{name: "alone for empty instructions", instruction: "   ", want: wantTurnProtocol},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := appendTurnProtocol(tc.instruction)
			if got != tc.want {
				t.Fatalf("appendTurnProtocol(%q) = %q, want %q", tc.instruction, got, tc.want)
			}
			if n := strings.Count(got, "Turn rules:"); n != 1 {
				t.Errorf("turn rules appear %d times, want once", n)
			}
			if !strings.HasSuffix(got, wantTurnProtocol) {
				t.Errorf("turn rules are not last: %q", got)
			}
		})
	}
}

func TestTurnProtocolNamesNoTool(t *testing.T) {
	for _, tool := range []string{"read_file", "shell.exec", "web_fetch", "workflow_propose"} {
		if strings.Contains(turnProtocol, tool) {
			t.Errorf("turn protocol names the tool %s", tool)
		}
	}
}

func TestTurnProtocolDoesNotChangeTheFingerprint(t *testing.T) {
	definition := &auraagent.Definition{ID: "main", Instructions: "Be helpful.", Tools: []string{"read_file"}, ModelRoute: "default"}
	soul := sha256.Sum256([]byte(definition.Instructions))

	fingerprint := definition.Fingerprint()

	if fingerprint.SoulSHA256 != hex.EncodeToString(soul[:]) {
		t.Fatalf("soul digest = %s, want the digest of the persona instructions alone", fingerprint.SoulSHA256)
	}
	assembled := sha256.Sum256([]byte(appendTurnProtocol(definition.Instructions)))
	if fingerprint.SoulSHA256 == hex.EncodeToString(assembled[:]) {
		t.Fatal("soul digest covers the turn protocol")
	}
}
