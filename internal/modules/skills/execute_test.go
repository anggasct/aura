package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeBroker struct {
	calls []*ScriptCall
	err   error
}

func (f *fakeBroker) RunScriptTool(_ context.Context, call *ScriptCall) (ScriptResult, error) {
	if f.err != nil {
		return ScriptResult{}, f.err
	}
	f.calls = append(f.calls, call)
	return ScriptResult{Output: []byte("ok"), Truncated: false}, nil
}

func makeExecRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "ops")
	writePackageFile(t, dir, "SKILL.md", "---\nname: ops\ndescription: Ops helpers.\nallowed-tools: fetch\n---\nBody.\n")
	writePackageFile(t, dir, filepath.Join("scripts", "run.sh"), "#!/bin/sh\necho hi\n")
	writePackageFile(t, dir, filepath.Join("references", "guide.md"), "# Guide\nSteps.\n")
	return root
}

func execEngine(t *testing.T, registry Registry, root string) *Engine {
	t.Helper()
	engine, err := NewEngine(registry, &EngineConfig{
		Dirs:                 []string{root},
		MaxIndexed:           16,
		MaxInstructionRunes:  8192,
		MaxResourceBytes:     65536,
		ScriptToolName:       "exec",
		ScriptToolCapability: "shell.execute",
		PolicyVersion:        "1",
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	if err := engine.Refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	return engine
}

func activateOps(t *testing.T, engine *Engine, registry *fakeRegistry, grants []string) Activation {
	t.Helper()
	var id, digest string
	for _, record := range registry.records {
		id, digest = record.ID, record.Digest
	}
	if _, err := engine.Accept(t.Context(), id, digest, grants); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if err := engine.Refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	activation, err := engine.Activate(t.Context(), "ops", "test")
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	return activation
}

func TestReadResource(t *testing.T) {
	root := makeExecRoot(t)
	registry := &fakeRegistry{}
	engine := execEngine(t, registry, root)
	activation := activateOps(t, engine, registry, nil)

	content, err := engine.ReadResource(t.Context(), &activation, "references/guide.md")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(content), "Steps.") {
		t.Errorf("content = %q", content)
	}
	for _, rel := range []string{"../SKILL.md", "/abs", "missing.md", "scripts/run.sh", "", "references/../SKILL.md"} {
		if _, err := engine.ReadResource(t.Context(), &activation, rel); err == nil {
			t.Errorf("path %q should fail", rel)
		}
	}
	writePackageFile(t, filepath.Join(root, "ops"), filepath.Join("references", "guide.md"), "# Mutated\n")
	if _, err := engine.ReadResource(t.Context(), &activation, "references/guide.md"); !isCode(err, ErrorCodeSkillDigestChanged) {
		t.Errorf("mutated err = %v", err)
	}
	stale := activation
	stale.Digest = "0000000000000000000000000000000000000000000000000000000000000000"
	if _, err := engine.ReadResource(t.Context(), &stale, "references/guide.md"); !isCode(err, ErrorCodeSkillUnavailable) {
		t.Errorf("stale err = %v", err)
	}
}

func TestRunScriptDeniedWithoutGrant(t *testing.T) {
	root := makeExecRoot(t)
	registry := &fakeRegistry{}
	engine := execEngine(t, registry, root)
	activation := activateOps(t, engine, registry, nil)

	broker := &fakeBroker{}
	staging := t.TempDir()
	_, err := engine.RunScript(t.Context(), broker, &activation, "scripts/run.sh", []string{"a"}, staging, nil)
	if !isCode(err, ErrorCodeSkillUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if len(broker.calls) != 0 {
		t.Errorf("broker must not be called without a grant, got %d calls", len(broker.calls))
	}
}

func TestRunScriptGranted(t *testing.T) {
	root := makeExecRoot(t)
	registry := &fakeRegistry{}
	engine := execEngine(t, registry, root)
	activation := activateOps(t, engine, registry, []string{"shell.execute"})

	broker := &fakeBroker{}
	staging := t.TempDir()
	result, err := engine.RunScript(t.Context(), broker, &activation, "scripts/run.sh", []string{"a", "b"}, staging, "approval-token")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if string(result.Output) != "ok" {
		t.Errorf("output = %q", result.Output)
	}
	if len(broker.calls) != 1 {
		t.Fatalf("calls = %d", len(broker.calls))
	}
	call := broker.calls[0]
	if call.ToolName != "exec" || call.Capability != "shell.execute" {
		t.Errorf("call = %+v", call)
	}
	if len(call.Args) != 2 || call.Approval != "approval-token" {
		t.Errorf("call = %+v", call)
	}
	info, err := os.Stat(call.Script)
	if err != nil {
		t.Fatalf("staged: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("staged mode = %o", info.Mode().Perm())
	}
	content, err := os.ReadFile(call.Script)
	if err != nil {
		t.Fatalf("read staged: %v", err)
	}
	if !strings.Contains(string(content), "echo hi") {
		t.Errorf("staged content = %q", content)
	}
	if _, err := engine.RunScript(t.Context(), broker, &activation, "references/guide.md", nil, staging, nil); !isCode(err, ErrorCodeSkillUnavailable) {
		t.Errorf("non-script err = %v", err)
	}
	if _, err := engine.RunScript(t.Context(), broker, &activation, "scripts/missing.sh", nil, staging, nil); !isCode(err, ErrorCodeSkillUnavailable) {
		t.Errorf("missing err = %v", err)
	}
	if _, err := engine.RunScript(t.Context(), nil, &activation, "scripts/run.sh", nil, staging, nil); !isCode(err, ErrorCodeInvalidArgument) {
		t.Errorf("nil broker err = %v", err)
	}
	if _, err := engine.RunScript(t.Context(), broker, &activation, "scripts/run.sh", nil, "/relative", nil); !isCode(err, ErrorCodeInvalidArgument) {
		t.Errorf("relative staging err = %v", err)
	}
}
