//go:build linux

package filesystem

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/platform/workspace"
)

func newTaintFSFixture(t *testing.T) (root string, mod *Module, store *workspace.TaintStore, adapters map[string]toolbroker.Adapter) {
	t.Helper()
	root = t.TempDir()
	db := sqlitetest.OpenMigrated(t, workspace.Schema())
	store = workspace.NewTaintStore(db)
	tracker := workspace.NewTracker(root, store, workspace.DefaultScanEntries, nil)
	mod, err := New(&Deps{Workspace: root, TaintTracker: tracker})
	if err != nil {
		t.Fatalf("filesystem.New: %v", err)
	}
	adapters = map[string]toolbroker.Adapter{}
	regs := mod.Tools()
	for i := range regs {
		adapters[regs[i].Definition.Name] = regs[i].Adapter
	}
	return root, mod, store, adapters
}

func fsReq(t *testing.T, name string, args map[string]any, tainted bool) *toolbroker.ToolRequest {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal %s args: %v", name, err)
	}
	req := &toolbroker.ToolRequest{
		ToolName:  name,
		SessionID: "sess-fs",
		Arguments: raw,
		Tainted:   tainted,
	}
	if tainted {
		req.TaintMark = turn.TaintMark{Source: turn.TaintSourceToolResult, Detail: "web_fetch"}
	}
	return req
}

func TestReadOfAFileWrittenWhileTaintedTaints(t *testing.T) {
	_, _, _, adapters := newTaintFSFixture(t)
	ctx := t.Context()

	if _, err := adapters[WriteFileToolName](ctx, fsReq(t, WriteFileToolName, map[string]any{
		"path": "clean.txt", "content": "owner note",
	}, false), approval.Constraints{}); err != nil {
		t.Fatalf("clean write: %v", err)
	}
	cleanRead, err := adapters[ReadFileToolName](ctx, fsReq(t, ReadFileToolName, map[string]any{"path": "clean.txt"}, false), approval.Constraints{})
	if err != nil || cleanRead.WorkspaceTainted || cleanRead.Untrusted {
		t.Fatalf("clean read = %+v %v, want untainted", cleanRead, err)
	}

	if _, err := adapters[WriteFileToolName](ctx, fsReq(t, WriteFileToolName, map[string]any{
		"path": "dirty.txt", "content": "from web page",
	}, true), approval.Constraints{}); err != nil {
		t.Fatalf("tainted write: %v", err)
	}
	dirtyRead, err := adapters[ReadFileToolName](ctx, fsReq(t, ReadFileToolName, map[string]any{"path": "dirty.txt"}, false), approval.Constraints{})
	if err != nil || !dirtyRead.WorkspaceTainted || !dirtyRead.Untrusted {
		t.Fatalf("dirty read = %+v %v, want tainted", dirtyRead, err)
	}

	listed, err := adapters[ListDirToolName](ctx, fsReq(t, ListDirToolName, map[string]any{"path": "."}, false), approval.Constraints{})
	if err != nil || !listed.WorkspaceTainted || !listed.Untrusted {
		t.Fatalf("list_dir with tainted file = %+v %v, want tainted", listed, err)
	}
}

func TestPartialReadComparesTheWholeFile(t *testing.T) {
	root, _, store, adapters := newTaintFSFixture(t)
	ctx := t.Context()

	fullContent := "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	if _, err := adapters[WriteFileToolName](ctx, fsReq(t, WriteFileToolName, map[string]any{
		"path": "orig.txt", "content": fullContent,
	}, true), approval.Constraints{}); err != nil {
		t.Fatalf("tainted write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "copy.txt"), []byte(fullContent), 0o600); err != nil {
		t.Fatalf("write copy.txt: %v", err)
	}
	if _, err := store.ClearPath(ctx, "orig.txt"); err != nil {
		t.Fatalf("ClearPath: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "orig2.txt"), []byte(fullContent), 0o600); err != nil {
		t.Fatalf("write orig2.txt: %v", err)
	}
	if _, err := adapters[WriteFileToolName](ctx, fsReq(t, WriteFileToolName, map[string]any{
		"path": "orig2.txt", "content": fullContent, "overwrite": true,
	}, true), approval.Constraints{}); err != nil {
		t.Fatalf("re-record tainted write: %v", err)
	}

	partialRead, err := adapters[ReadFileToolName](ctx, fsReq(t, ReadFileToolName, map[string]any{
		"path": "copy.txt", "max_bytes": 10,
	}, false), approval.Constraints{})
	if err != nil {
		t.Fatalf("partial read copy.txt: %v", err)
	}
	if !partialRead.WorkspaceTainted || !partialRead.Untrusted {
		t.Fatalf("partial read of digest-matching copy = %+v, want tainted by whole-file sha256", partialRead)
	}
}

func TestCleanAppendKeepsTheFileTainted(t *testing.T) {
	root, _, _, adapters := newTaintFSFixture(t)
	ctx := t.Context()

	if _, err := adapters[WriteFileToolName](ctx, fsReq(t, WriteFileToolName, map[string]any{
		"path": "log.txt", "content": "tainted line\n",
	}, true), approval.Constraints{}); err != nil {
		t.Fatalf("tainted write: %v", err)
	}
	f, err := os.OpenFile(filepath.Join(root, "log.txt"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := f.WriteString("clean line\n"); err != nil {
		_ = f.Close()
		t.Fatalf("append: %v", err)
	}
	_ = f.Close()

	res, err := adapters[ReadFileToolName](ctx, fsReq(t, ReadFileToolName, map[string]any{"path": "log.txt"}, false), approval.Constraints{})
	if err != nil || !res.WorkspaceTainted {
		t.Fatalf("read after clean append = %+v %v, want tainted", res, err)
	}
}

func TestHostEditKeepsTheRecordedPathTainted(t *testing.T) {
	root, _, _, adapters := newTaintFSFixture(t)
	ctx := t.Context()

	if _, err := adapters[WriteFileToolName](ctx, fsReq(t, WriteFileToolName, map[string]any{
		"path": "notes.txt", "content": "original tainted text",
	}, true), approval.Constraints{}); err != nil {
		t.Fatalf("tainted write: %v", err)
	}

	tmp := filepath.Join(root, "notes.txt.tmp")
	if err := os.WriteFile(tmp, []byte("host replaced content with new inode"), 0o600); err != nil {
		t.Fatalf("write tmp: %v", err)
	}
	if err := os.Rename(tmp, filepath.Join(root, "notes.txt")); err != nil {
		t.Fatalf("atomic rename: %v", err)
	}

	res, err := adapters[ReadFileToolName](ctx, fsReq(t, ReadFileToolName, map[string]any{"path": "notes.txt"}, false), approval.Constraints{})
	if err != nil || !res.WorkspaceTainted {
		t.Fatalf("read after host atomic replace = %+v %v, want tainted by recorded path", res, err)
	}
}

func TestRenameAndAppendKeepsTheTaint(t *testing.T) {
	root, _, _, adapters := newTaintFSFixture(t)
	ctx := t.Context()

	if _, err := adapters[WriteFileToolName](ctx, fsReq(t, WriteFileToolName, map[string]any{
		"path": "before.txt", "content": "secret payload",
	}, true), approval.Constraints{}); err != nil {
		t.Fatalf("tainted write: %v", err)
	}
	renamed := filepath.Join(root, "after.txt")
	if err := os.Rename(filepath.Join(root, "before.txt"), renamed); err != nil {
		t.Fatalf("rename: %v", err)
	}
	f, err := os.OpenFile(renamed, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open renamed: %v", err)
	}
	_, _ = f.WriteString(" + appended")
	_ = f.Close()

	res, err := adapters[ReadFileToolName](ctx, fsReq(t, ReadFileToolName, map[string]any{"path": "after.txt"}, false), approval.Constraints{})
	if err != nil || !res.WorkspaceTainted {
		t.Fatalf("read after rename+append = %+v %v, want tainted by (device, inode)", res, err)
	}
}

func TestCleanOverwriteClearsTheRecord(t *testing.T) {
	_, _, store, adapters := newTaintFSFixture(t)
	ctx := t.Context()

	if _, err := adapters[WriteFileToolName](ctx, fsReq(t, WriteFileToolName, map[string]any{
		"path": "draft.txt", "content": "untrusted draft",
	}, true), approval.Constraints{}); err != nil {
		t.Fatalf("tainted write: %v", err)
	}
	if _, err := adapters[WriteFileToolName](ctx, fsReq(t, WriteFileToolName, map[string]any{
		"path": "draft.txt", "content": "clean rewrite", "overwrite": true,
	}, false), approval.Constraints{}); err != nil {
		t.Fatalf("clean overwrite: %v", err)
	}
	rows, err := store.List(ctx, "draft.txt")
	if err != nil || len(rows) != 0 {
		t.Fatalf("taint rows after clean overwrite = %+v %v, want 0", rows, err)
	}
	res, err := adapters[ReadFileToolName](ctx, fsReq(t, ReadFileToolName, map[string]any{"path": "draft.txt"}, false), approval.Constraints{})
	if err != nil || res.WorkspaceTainted || res.Untrusted {
		t.Fatalf("read after clean overwrite = %+v %v, want clean", res, err)
	}
}
