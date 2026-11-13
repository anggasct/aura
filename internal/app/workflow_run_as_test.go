package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/modules/workflow"
)

const readerPersonaConfig = `agents:
  definitions:
    - id: reader
      description: reads files
      instructions: read
      tools: [read_file]
`

func writeRunAsFile(t *testing.T, runAs, tool, args string) string {
	t.Helper()
	body := "id: scoped\nversion: 1\ngoal: Use a tool\nsource: defined\nrun_as: " + runAs + "\nsteps:\n" +
		"  - id: gate\n    timeout: 1m\n    executor: { kind: approval }\n" +
		"  - id: save\n    depends_on: [gate]\n    timeout: 1m\n    executor:\n      kind: tool\n      tool: " + tool + "\n      args: " + args + "\n"
	file := filepath.Join(t.TempDir(), "scoped.yaml")
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return file
}

func TestWorkflowValidateChecksTheRunAsCeiling(t *testing.T) {
	gf := writeWorkflowFixturesWith(t, "", readerPersonaConfig)
	tests := []struct {
		name     string
		runAs    string
		tool     string
		args     string
		wantCode workflow.ErrorCode
		wantStep string
	}{
		{name: "a tool the persona lists", runAs: "reader", tool: "read_file", args: `{ path: a }`},
		{name: "a tool the persona does not list", runAs: "reader", tool: "write_file", args: `{ path: a, content: b }`, wantCode: workflow.ErrorCodeCapabilityExceeded, wantStep: `"save"`},
		{name: "a persona that does not exist", runAs: "ghost", tool: "read_file", args: `{ path: a }`, wantCode: workflow.ErrorCodeSpecInvalid, wantStep: "run_as"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runWorkflowCommand(t, gf, "validate", writeRunAsFile(t, tc.runAs, tc.tool, tc.args))
			if tc.wantCode == "" {
				if err != nil || !strings.Contains(out, "valid: scoped") {
					t.Fatalf("validate = %q, %v, want valid", out, err)
				}
				return
			}
			if code, ok := workflow.CodeOf(err); !ok || code != tc.wantCode {
				t.Fatalf("validate = %v, want %s", err, tc.wantCode)
			}
			if !strings.Contains(err.Error(), tc.wantStep) {
				t.Fatalf("validate error does not mention %s: %v", tc.wantStep, err)
			}
		})
	}
}
