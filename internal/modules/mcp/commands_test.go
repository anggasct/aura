package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/fault"
)

func executeMCPCommand(t *testing.T, open Opener, args ...string) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: "aura", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(Commands(open)...)
	var out bytes.Buffer
	var errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	err := root.ExecuteContext(t.Context())
	return out.String(), err
}

func TestMCPCommandsRefusesNilOpener(t *testing.T) {
	cases := [][]string{
		{"mcp", "list"},
		{"mcp", "trust", "approve", "docs"},
		{"mcp", "trust", "revoke", "docs"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			_, err := executeMCPCommand(t, nil, args...)
			if !fault.HasCode(err, ErrConfigInvalid) {
				t.Fatalf("args %v error = %v, want %s", args, err, ErrConfigInvalid)
			}
		})
	}
}

func TestMCPCommandsListAndTrustRoundTrip(t *testing.T) {
	registry, err := NewFileTrustRegistry(filepath.Join(t.TempDir(), "mcp", "trust.json"))
	if err != nil {
		t.Fatalf("NewFileTrustRegistry: %v", err)
	}
	servers := []ServerConfig{
		{Name: "docs", Transport: TransportStdio, Command: "/usr/bin/docs-mcp"},
	}
	open := func(cmd *cobra.Command, fn func(ctx context.Context, servers []ServerConfig, registry *FileTrustRegistry) error) error {
		return fn(cmd.Context(), servers, registry)
	}

	out, err := executeMCPCommand(t, open, "mcp", "list", "--json")
	if err != nil {
		t.Fatalf("list --json: %v", err)
	}
	var decoded ListOutput
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded.Servers) != 1 || decoded.Servers[0].Name != "docs" || decoded.Servers[0].State != string(TrustStateRequired) {
		t.Fatalf("decoded = %+v", decoded)
	}

	approveOut, err := executeMCPCommand(t, open, "mcp", "trust", "approve", "docs")
	if err != nil || !strings.Contains(approveOut, "approved docs") {
		t.Fatalf("approve = %q, %v", approveOut, err)
	}

	revokeOut, err := executeMCPCommand(t, open, "mcp", "trust", "revoke", "docs")
	if err != nil || !strings.Contains(revokeOut, "revoked trust for docs") {
		t.Fatalf("revoke = %q, %v", revokeOut, err)
	}
}
