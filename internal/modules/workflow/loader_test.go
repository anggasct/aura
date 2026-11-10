package workflow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParseSpecLoadsToolArgs(t *testing.T) {
	spec, err := parseSpec([]byte(`id: toolfile
version: 1
goal: Load tool args from file
source: defined
steps:
  - id: read
    executor:
      kind: tool
      tool: read_file
      args:
        path: notes.txt
        limit: 10
        recursive: true
    timeout: 1m
`))
	if err != nil {
		t.Fatalf("parseSpec: %v", err)
	}
	if len(spec.Steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(spec.Steps))
	}
	var args map[string]any
	if err := json.Unmarshal(spec.Steps[0].Executor.ToolArgs, &args); err != nil {
		t.Fatalf("unmarshal args: %v (%s)", err, spec.Steps[0].Executor.ToolArgs)
	}
	if args["path"] != "notes.txt" {
		t.Fatalf("args[path] = %v, want notes.txt", args["path"])
	}
	if err := Validate(spec, testValidationDeps()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	roundTripped, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	var decoded Spec
	if err := json.Unmarshal(roundTripped, &decoded); err != nil {
		t.Fatalf("unmarshal spec: %v", err)
	}
	if string(decoded.Steps[0].Executor.ToolArgs) != string(spec.Steps[0].Executor.ToolArgs) {
		t.Fatalf("round-trip args = %s, want %s", decoded.Steps[0].Executor.ToolArgs, spec.Steps[0].Executor.ToolArgs)
	}
}

func TestParseSpecRejectsNonObjectToolArgs(t *testing.T) {
	for _, body := range []string{
		"args: [1, 2]",
		"args: hello",
		"args: 42",
	} {
		content := "id: toolfile\nversion: 1\ngoal: g\nsource: defined\nsteps:\n  - id: read\n    executor:\n      kind: tool\n      tool: read_file\n      " + body + "\n    timeout: 1m\n"
		if _, err := parseSpec([]byte(content)); err == nil {
			t.Fatalf("parseSpec accepted %q", body)
		} else if code, _ := CodeOf(err); code != ErrorCodeExecutorInvalid {
			t.Fatalf("code = %s (%v), want %s", code, err, ErrorCodeExecutorInvalid)
		}
	}
}

func TestLoadSpecFileLoadsToolArgs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tool.yaml")
	content := "id: toolfile\nversion: 1\ngoal: Load tool args from file\nsource: defined\nsteps:\n  - id: read\n    executor:\n      kind: tool\n      tool: read_file\n      args:\n        path: notes.txt\n    timeout: 1m\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	spec, err := LoadSpecFile(path)
	if err != nil {
		t.Fatalf("LoadSpecFile: %v", err)
	}
	var args map[string]any
	if err := json.Unmarshal(spec.Steps[0].Executor.ToolArgs, &args); err != nil {
		t.Fatalf("unmarshal args: %v", err)
	}
	if args["path"] != "notes.txt" {
		t.Fatalf("args[path] = %v, want notes.txt", args["path"])
	}
	if err := Validate(spec, testValidationDeps()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}
