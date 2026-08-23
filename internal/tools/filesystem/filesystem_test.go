//go:build linux

package filesystem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func TestWriteFileFsyncsFileAndParentDirectoryOnBothPaths(t *testing.T) {
	workspace := t.TempDir()
	adapters, err := New(Options{Workspace: workspace, MaxFileBytes: 1024, MaxDirEntries: 10})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	write := adapters["write_file@v1"]
	original := sysFsync
	t.Cleanup(func() { sysFsync = original })

	steps := []struct {
		name    string
		request map[string]any
	}{
		{"create-new", map[string]any{"path": "note.txt", "content": "one"}},
		{"replace", map[string]any{"path": "note.txt", "content": "two", "overwrite": true}},
	}
	for _, step := range steps {
		var fsyncs []int
		sysFsync = func(fd int) error {
			fsyncs = append(fsyncs, fd)
			return original(fd)
		}
		if _, err := write(context.Background(), fsRequest(t, step.request), approvalConstraints()); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		sysFsync = original
		if len(fsyncs) != 2 {
			t.Fatalf("%s fsyncs = %v, want file then parent directory", step.name, fsyncs)
		}
		if fsyncs[0] == fsyncs[1] {
			t.Fatalf("%s fsynced the same descriptor twice: %v", step.name, fsyncs)
		}
	}
	if got, err := os.ReadFile(filepath.Join(workspace, "note.txt")); err != nil || string(got) != "two" {
		t.Fatalf("final content = %q, err = %v", got, err)
	}
}

func TestWriteFileCreateNewPropagatesCloseError(t *testing.T) {
	workspace := t.TempDir()
	adapters, err := New(Options{Workspace: workspace, MaxFileBytes: 1024, MaxDirEntries: 10})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	original := sysClose
	sysClose = func(fd int) error {
		_ = original(fd)
		return errors.New("close failed")
	}
	t.Cleanup(func() { sysClose = original })
	_, err = adapters["write_file@v1"](context.Background(), fsRequest(t, map[string]any{"path": "note.txt", "content": "one"}), approvalConstraints())
	if err == nil {
		t.Fatal("write succeeded despite close failure")
	}
	if _, statErr := os.Lstat(filepath.Join(workspace, "note.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("failed create was not rolled back: %v", statErr)
	}
}

func TestWriteFilePropagatesParentSyncError(t *testing.T) {
	workspace := t.TempDir()
	adapters, err := New(Options{Workspace: workspace, MaxFileBytes: 1024, MaxDirEntries: 10})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	write := adapters["write_file@v1"]
	original := sysFsync
	t.Cleanup(func() { sysFsync = original })

	for _, step := range []struct {
		name    string
		request map[string]any
	}{
		{"create-new", map[string]any{"path": "note.txt", "content": "one"}},
		{"replace", map[string]any{"path": "note.txt", "content": "two", "overwrite": true}},
	} {
		calls := 0
		sysFsync = func(fd int) error {
			calls++
			if calls == 2 {
				return errors.New("directory sync failed")
			}
			return original(fd)
		}
		if _, err := write(context.Background(), fsRequest(t, step.request), approvalConstraints()); err == nil {
			t.Fatalf("%s succeeded despite parent directory sync failure", step.name)
		}
	}
	sysFsync = original
}

func TestListDirBoundsLargeDirectoryWithoutFullListing(t *testing.T) {
	workspace := t.TempDir()
	const total = 3000
	const limit = 100
	for i := range total {
		if err := os.WriteFile(filepath.Join(workspace, fmt.Sprintf("file-%04d", i)), []byte("x"), 0o600); err != nil {
			t.Fatalf("seed file: %v", err)
		}
	}
	adapters, err := New(Options{Workspace: workspace, MaxFileBytes: 1024, MaxDirEntries: limit})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := adapters["list_dir@v1"](context.Background(), fsRequest(t, map[string]any{"path": "."}), approvalConstraints())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var decoded directoryResult
	if err := json.Unmarshal(result.Output, &decoded); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(decoded.Entries) != limit || !decoded.Truncated {
		t.Fatalf("entries = %d, truncated = %v, want %d entries with truncation", len(decoded.Entries), decoded.Truncated, limit)
	}
}

func TestListDirExactFitIsNotTruncated(t *testing.T) {
	workspace := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("seed file: %v", err)
		}
	}
	adapters, err := New(Options{Workspace: workspace, MaxFileBytes: 1024, MaxDirEntries: 3})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := adapters["list_dir@v1"](context.Background(), fsRequest(t, map[string]any{"path": "."}), approvalConstraints())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var decoded directoryResult
	if err := json.Unmarshal(result.Output, &decoded); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(decoded.Entries) != 3 || decoded.Truncated {
		t.Fatalf("entries = %d, truncated = %v, want exactly 3 without truncation", len(decoded.Entries), decoded.Truncated)
	}
}

func TestListDirRecursiveSharesBudgetAcrossDirectories(t *testing.T) {
	workspace := t.TempDir()
	nested := filepath.Join(workspace, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, path := range []string{
		filepath.Join(workspace, "top-a"),
		filepath.Join(workspace, "top-b"),
		filepath.Join(nested, "child-a"),
		filepath.Join(nested, "child-b"),
	} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatalf("seed file: %v", err)
		}
	}
	adapters, err := New(Options{Workspace: workspace, MaxFileBytes: 1024, MaxDirEntries: 3})
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
	if len(decoded.Entries) != 3 || !decoded.Truncated {
		t.Fatalf("entries = %d, truncated = %v, want the shared 3-entry budget with truncation", len(decoded.Entries), decoded.Truncated)
	}
	for _, entry := range decoded.Entries {
		if strings.Contains(entry.Path, "..") {
			t.Fatalf("entry escaped the workspace: %+v", entry)
		}
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
