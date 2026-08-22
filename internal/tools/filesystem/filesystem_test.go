package filesystem

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/toolbroker"
)

func TestReadFileRejectsWorkspaceEscapes(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o600); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(workspace, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	adapters, err := New(Options{Workspace: workspace, MaxFileBytes: 1024, MaxDirEntries: 10})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	read := adapters["read_file@v1"]
	for _, path := range []string{"../secret", filepath.Join(outside, "secret"), "link"} {
		request := fsRequest(t, map[string]any{"path": path})
		_, err := read(context.Background(), request, approvalConstraints())
		if class := classOf(err); class != toolbroker.ResultPolicyDenied {
			t.Errorf("path %q class = %q, err = %v", path, class, err)
		}
	}
}

func TestWriteFileIsAtomicAndRejectsHardLinks(t *testing.T) {
	workspace := t.TempDir()
	adapters, err := New(Options{Workspace: workspace, MaxFileBytes: 1024, MaxDirEntries: 10})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	write := adapters["write_file@v1"]
	request := fsRequest(t, map[string]any{"path": "note.txt", "content": "one"})
	if _, err := write(context.Background(), request, approvalConstraints()); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(workspace, "note.txt")); err != nil || string(got) != "one" {
		t.Fatalf("created content = %q, err = %v", got, err)
	}
	request = fsRequest(t, map[string]any{"path": "note.txt", "content": "two"})
	result, writeErr := write(context.Background(), request, approvalConstraints())
	if class := classOfError(&result, writeErr); class != toolbroker.ResultPolicyDenied {
		t.Fatalf("overwrite=false class = %q", class)
	}
	request = fsRequest(t, map[string]any{"path": "note.txt", "content": "two", "overwrite": true})
	if _, err := write(context.Background(), request, approvalConstraints()); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(workspace, "note.txt")); string(got) != "two" {
		t.Fatalf("replaced content = %q", got)
	}
	if err := os.Link(filepath.Join(workspace, "note.txt"), filepath.Join(workspace, "alias.txt")); err != nil {
		t.Fatalf("hard link: %v", err)
	}
	request = fsRequest(t, map[string]any{"path": "alias.txt", "content": "three", "overwrite": true})
	result, writeErr = write(context.Background(), request, approvalConstraints())
	if class := classOfError(&result, writeErr); class != toolbroker.ResultPolicyDenied {
		t.Fatalf("hard-link class = %q", class)
	}
}

func TestListDirDoesNotFollowSymlink(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o600); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "outside")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	adapters, err := New(Options{Workspace: workspace, MaxFileBytes: 1024, MaxDirEntries: 10})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := adapters["list_dir@v1"](context.Background(), fsRequest(t, map[string]any{"path": ".", "recursive": true}), approvalConstraints())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var decoded directoryResult
	if err := json.Unmarshal(result.Output, &decoded); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(decoded.Entries) != 1 || decoded.Entries[0].Kind != "symlink" {
		t.Fatalf("entries = %+v", decoded.Entries)
	}
}

func fsRequest(t *testing.T, value map[string]any) *toolbroker.ToolRequest {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return &toolbroker.ToolRequest{ToolName: "read_file", ToolVersion: "v1", Arguments: data}
}

func approvalConstraints() approval.Constraints { return approval.Constraints{} }

func classOfError(result *toolbroker.ToolResult, err error) toolbroker.ResultClass {
	if err == nil {
		return result.Class
	}
	class, _ := toolbroker.CodeOf(err)
	return class
}

func classOf(err error) toolbroker.ResultClass {
	class, _ := toolbroker.CodeOf(err)
	return class
}
