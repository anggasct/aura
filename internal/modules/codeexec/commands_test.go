package codeexec

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/modules/codeexec/interpreter"
	"github.com/anggasct/aura/internal/platform/fault"
)

func TestCommandsRejectNilOpener(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"install", []string{"install", "python", "--archive", "x.tar.gz"}},
		{"list", []string{"list"}},
		{"remove", []string{"remove", "python"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmds := Commands(nil)
			if len(cmds) != 1 {
				t.Fatalf("Commands(nil) returned %d commands, want 1", len(cmds))
			}
			cmd := cmds[0]
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			cmd.SetArgs(tc.args)
			err := cmd.ExecuteContext(t.Context())
			if !fault.HasCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("ExecuteContext(%v) error = %v, want %s", tc.args, err, ErrorCodeInvalidArgument)
			}
		})
	}
}

func TestCommandsRejectNilStore(t *testing.T) {
	cmd := Commands(func(cmd *cobra.Command, fn func(ctx context.Context, logger *slog.Logger, store *interpreter.Store) error) error {
		return fn(cmd.Context(), nil, nil)
	})[0]
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"list"})
	err := cmd.ExecuteContext(t.Context())
	if !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("list with nil store error = %v, want %s", err, ErrorCodeInvalidArgument)
	}
}

func TestCommandsInstallListAndRemove(t *testing.T) {
	archive := rigArchive(t, "python")
	dir := t.TempDir()
	dataRoot := filepath.Join(dir, "data")
	t.Cleanup(func() { removeTree(dataRoot) })
	archivePath := filepath.Join(dir, "python.tar.gz")
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := rigManifest(t, map[string][]byte{"python": archive}, []rigRuntime{{id: "python", version: "1.0.0"}})
	store, err := interpreter.NewStore(dataRoot, manifest)
	if err != nil {
		t.Fatal(err)
	}
	open := func(cmd *cobra.Command, fn func(ctx context.Context, logger *slog.Logger, s *interpreter.Store) error) error {
		return fn(cmd.Context(), slog.New(slog.DiscardHandler), store)
	}
	run := func(args ...string) (string, error) {
		cmd := Commands(open)[0]
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SilenceErrors, cmd.SilenceUsage = true, true
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(t.Context())
		return out.String(), err
	}

	out, err := run("install", "python", "--archive", archivePath)
	if err != nil || !strings.Contains(out, "installed: python 1.0.0") {
		t.Fatalf("install = %q, %v", out, err)
	}
	out, err = run("list", "--json")
	if err != nil {
		t.Fatalf("list --json: %v", err)
	}
	var listed ListOutput
	if err := json.Unmarshal([]byte(out), &listed); err != nil || len(listed.Runtimes) != 1 || listed.Runtimes[0].State != RuntimeStateInstalled {
		t.Fatalf("list --json = %q, %+v, %v", out, listed, err)
	}
	out, err = run("remove", "python")
	if err != nil || !strings.Contains(out, "dry-run: would remove python") {
		t.Fatalf("remove dry-run = %q, %v", out, err)
	}
	out, err = run("remove", "python", "--apply")
	if err != nil || !strings.Contains(out, "removed: python") {
		t.Fatalf("remove --apply = %q, %v", out, err)
	}
}
