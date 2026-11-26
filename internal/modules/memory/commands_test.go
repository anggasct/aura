package memory

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/fault"
)

func TestMemoryCommandsRefuseNilOpener(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"inspect", []string{"memory", "inspect"}},
		{"forget", []string{"memory", "forget", "--session", "sess-1"}},
		{"purge", []string{"memory", "purge"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := &cobra.Command{Use: "aura", SilenceUsage: true, SilenceErrors: true}
			root.AddCommand(Commands(nil)...)
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			root.SetArgs(tc.args)
			err := root.ExecuteContext(t.Context())
			if !fault.HasCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("Execute(%v) = %v, want %q", tc.args, err, ErrorCodeInvalidArgument)
			}
		})
	}
}

func TestMemoryInspectLinesNameThePersona(t *testing.T) {
	lines := memoryInspectLines([]StoredDocument{{
		ID: "mem-1", AgentID: "kuma", SessionID: "kuma/discord:home/dm:1", Kind: KindEventText,
		FromSequence: 1, ToSequence: 1, TrustLabel: "owner_input", Content: "hello",
	}})
	if len(lines) != 1 || !strings.Contains(lines[0], "mem-1 agent=kuma session=kuma/discord:home/dm:1 ") {
		t.Fatalf("lines = %q, want the persona after the document id", lines)
	}
}
