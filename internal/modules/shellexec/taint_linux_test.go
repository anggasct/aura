//go:build linux

package shellexec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/sandbox"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/workspace"
)

func hostTestRunner(ctx context.Context, spec *sandbox.Spec, command string, args ...string) (sandbox.Result, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = spec.WorkingDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if !os.IsNotExist(err) && cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
			err = nil
		}
	}
	return sandbox.Result{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: exitCode}, err
}

func newExecTaintRig(t *testing.T, maxEntries int) (root string, store *workspace.TaintStore, tracker *workspace.Tracker, adapter toolbroker.Adapter) {
	t.Helper()
	root = t.TempDir()
	db := sqlitetest.OpenMigrated(t, workspace.Schema())
	store = workspace.NewTaintStore(db)
	tracker = workspace.NewTracker(root, store, maxEntries, nil, slog.New(slog.DiscardHandler))
	var err error
	adapter, err = NewAdapter(&Options{
		Workspace:      root,
		Timeout:        10 * time.Second,
		MaxStdoutBytes: 64 << 10,
		MaxStderrBytes: 64 << 10,
		TaintTracker:   tracker,
		runner:         hostTestRunner,
	})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	return root, store, tracker, adapter
}

func execReq(t *testing.T, script string, tainted bool) *toolbroker.ToolRequest {
	t.Helper()
	raw, err := json.Marshal(struct {
		Command []string `json:"command"`
	}{
		Command: []string{"/bin/sh", "-c", script},
	})
	if err != nil {
		t.Fatalf("marshal exec args: %v", err)
	}
	req := &toolbroker.ToolRequest{
		ToolName:    "exec",
		ToolVersion: "v1",
		SessionID:   "sess-exec",
		Arguments:   raw,
		Tainted:     tainted,
	}
	if tainted {
		req.TaintSource = "web_fetch"
	}
	return req
}

func TestFilesChangedByATaintedExecTaint(t *testing.T) {
	root, store, tracker, adapter := newExecTaintRig(t, workspace.DefaultScanEntries)
	ctx := t.Context()

	res, err := adapter(ctx, execReq(t, "printf 'from-exec' > generated.txt", true), approval.Constraints{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("tainted exec: %v", err)
	}
	if !res.WorkspaceTainted || !res.Untrusted {
		t.Fatalf("tainted exec result = %+v, want tainted", res)
	}
	rows, err := store.List(ctx, "")
	if err != nil || len(rows) != 1 || rows[0].Path != "generated.txt" {
		t.Fatalf("workspace_taint rows = %+v %v, want generated.txt", rows, err)
	}
	if tracker.WalkCount() != 1 {
		t.Errorf("walk count = %d, want 1", tracker.WalkCount())
	}
	if _, err := os.Stat(filepath.Join(root, "generated.txt")); err != nil {
		t.Fatalf("generated.txt missing: %v", err)
	}
}

func TestCleanExecDerivedFileIsRecordedWhileRecordsExist(t *testing.T) {
	root, store, _, adapter := newExecTaintRig(t, workspace.DefaultScanEntries)
	ctx := t.Context()

	if _, err := adapter(ctx, execReq(t, "printf 'seed' > seed.txt", true), approval.Constraints{Timeout: 10 * time.Second}); err != nil {
		t.Fatalf("tainted seed exec: %v", err)
	}
	res, err := adapter(ctx, execReq(t, "cp seed.txt derived.txt", false), approval.Constraints{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("clean derived exec: %v", err)
	}
	if !res.WorkspaceTainted {
		t.Fatalf("clean exec while records exist = %+v, want WorkspaceTainted=true", res)
	}
	rows, err := store.List(ctx, "derived.txt")
	if err != nil || len(rows) != 1 {
		t.Fatalf("derived.txt rows = %+v %v, want 1", rows, err)
	}
	if _, err := os.Stat(filepath.Join(root, "derived.txt")); err != nil {
		t.Fatalf("derived.txt missing: %v", err)
	}
}

func TestExecWithoutRecordsDoesNotWalkTheWorkspace(t *testing.T) {
	_, _, tracker, adapter := newExecTaintRig(t, workspace.DefaultScanEntries)
	ctx := t.Context()

	res, err := adapter(ctx, execReq(t, "printf 'ok'", false), approval.Constraints{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("clean exec: %v", err)
	}
	if res.WorkspaceTainted || res.Untrusted {
		t.Fatalf("clean exec on empty table = %+v, want untainted", res)
	}
	if tracker.WalkCount() != 0 {
		t.Fatalf("walk count on clean exec with empty table = %d, want 0", tracker.WalkCount())
	}
}

func TestDeletedRecordedFileStopsTaintingExec(t *testing.T) {
	_, store, tracker, adapter := newExecTaintRig(t, workspace.DefaultScanEntries)
	ctx := t.Context()

	if _, err := adapter(ctx, execReq(t, "printf 'temp' > temp.txt", true), approval.Constraints{Timeout: 10 * time.Second}); err != nil {
		t.Fatalf("tainted write exec: %v", err)
	}
	rmRes, err := adapter(ctx, execReq(t, "rm temp.txt", false), approval.Constraints{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("rm exec: %v", err)
	}
	if !rmRes.WorkspaceTainted {
		t.Fatalf("rm exec (started with record) = %+v, want WorkspaceTainted=true", rmRes)
	}
	has, err := store.HasRecords(ctx)
	if err != nil || has {
		t.Fatalf("HasRecords after deleting recorded file = %v %v, want false", has, err)
	}

	walksBefore := tracker.WalkCount()
	nextRes, err := adapter(ctx, execReq(t, "printf 'clean-again'", false), approval.Constraints{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("next clean exec: %v", err)
	}
	if nextRes.WorkspaceTainted || nextRes.Untrusted {
		t.Fatalf("next exec after record cleared = %+v, want clean", nextRes)
	}
	if tracker.WalkCount() != walksBefore {
		t.Errorf("walk count increased from %d to %d after table became empty", walksBefore, tracker.WalkCount())
	}
}

func TestExecOutputTaintsWhileTheWorkspaceHasRecords(t *testing.T) {
	_, _, _, adapter := newExecTaintRig(t, workspace.DefaultScanEntries)
	ctx := t.Context()

	if _, err := adapter(ctx, execReq(t, "printf 'untrusted' > tainted.txt", true), approval.Constraints{Timeout: 10 * time.Second}); err != nil {
		t.Fatalf("tainted exec: %v", err)
	}
	res, err := adapter(ctx, execReq(t, "cat tainted.txt", false), approval.Constraints{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("clean read exec: %v", err)
	}
	if !res.WorkspaceTainted || !res.Untrusted {
		t.Fatalf("clean exec output while records exist = %+v, want tainted", res)
	}
}

func TestExecScanPastTheBoundTaintsTheWorkspace(t *testing.T) {
	root, store, _, adapter := newExecTaintRig(t, 3)
	ctx := t.Context()

	for i := range 5 {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%d.txt", i)), []byte("x"), 0o600); err != nil {
			t.Fatalf("seed file %d: %v", i, err)
		}
	}
	res, err := adapter(ctx, execReq(t, "printf 'new' > extra.txt", true), approval.Constraints{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("tainted exec over bound: %v", err)
	}
	if !res.WorkspaceTainted {
		t.Fatalf("result = %+v, want WorkspaceTainted=true", res)
	}
	wide, err := store.HasWorkspaceWide(ctx)
	if err != nil || !wide {
		t.Fatalf("HasWorkspaceWide = %v %v, want true when walk exceeds bound", wide, err)
	}
}
