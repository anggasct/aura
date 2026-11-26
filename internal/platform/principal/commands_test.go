package principal

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/clikit"
	"github.com/anggasct/aura/internal/platform/fault"
)

func TestCommandsNilOpenerReturnsInvalidArgument(t *testing.T) {
	cases := [][]string{
		{"grants", "list"},
		{"grants", "revoke", "g-1"},
		{"apikey", "create", "--persona", "main", "--class", "trusted", "--label", "dev"},
		{"apikey", "list"},
		{"apikey", "revoke", "aaaaaaaaaaaa"},
		{"pairing", "list"},
		{"pairing", "approve", "discord", "12345678"},
		{"pairing", "revoke", "discord", "12345"},
		{"pairing", "clear-pending"},
		{"principal", "list"},
		{"principal", "show", "discord:12345"},
		{"principal", "forget", "discord:12345"},
		{"owner", "whoami"},
		{"owner", "rekey"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args[:2], "_"), func(t *testing.T) {
			root := &cobra.Command{Use: "aura"}
			root.AddCommand(Commands(nil)...)
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs(args)
			err := root.ExecuteContext(t.Context())
			if !fault.HasCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("Execute(%v) = %v, want %s", args, err, ErrorCodeInvalidArgument)
			}
		})
	}
}

func TestPrincipalCommandsUsageErrors(t *testing.T) {
	cases := [][]string{
		{"apikey", "create", "--persona", "main", "--class", "owner", "--label", "ops"},
		{"pairing", "approve", "slack", "12345678"},
		{"principal", "list", "--class", "owner"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args[:2], "_"), func(t *testing.T) {
			root := &cobra.Command{Use: "aura"}
			root.AddCommand(Commands(nil)...)
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs(args)
			err := root.ExecuteContext(t.Context())
			if !clikit.IsUsageError(err) {
				t.Fatalf("Execute(%v) = %v, want clikit.UsageError", args, err)
			}
		})
	}
}

func TestPairingServiceNilGuards(t *testing.T) {
	if svc := NewPairingService(nil, nil, nil); svc != nil {
		t.Fatalf("NewPairingService(nil, nil, nil) = %v, want nil", svc)
	}
	var nilSvc *PairingService
	if _, err := nilSvc.Granted(t.Context(), "discord:1", "office"); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("nil Granted = %v, want %s", err, ErrorCodeInvalidArgument)
	}
	if _, err := nilSvc.Request(t.Context(), "discord:1", "office"); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("nil Request = %v, want %s", err, ErrorCodeInvalidArgument)
	}
	empty := &PairingService{}
	if _, err := empty.Granted(t.Context(), "discord:1", "office"); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("empty Granted = %v, want %s", err, ErrorCodeInvalidArgument)
	}
	if _, err := empty.Request(t.Context(), "discord:1", "office"); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("empty Request = %v, want %s", err, ErrorCodeInvalidArgument)
	}
}
