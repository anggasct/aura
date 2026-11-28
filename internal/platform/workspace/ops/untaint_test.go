package workspaceops

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/workspace"
)

func TestWorkspaceUntaintClearsRecords(t *testing.T) {
	ctx := t.Context()
	db := operationtest.OpenDatabase(t, workspace.Schema())
	store := workspace.NewTaintStore(db)
	if err := store.RecordFile(ctx, "a.txt", "1111111111111111111111111111111111111111111111111111111111111111", 1, 101, "web_fetch", "sess-1"); err != nil {
		t.Fatalf("RecordFile a.txt: %v", err)
	}
	if err := store.RecordFile(ctx, "b.txt", "2222222222222222222222222222222222222222222222222222222222222222", 1, 102, "web_search", "sess-1"); err != nil {
		t.Fatalf("RecordFile b.txt: %v", err)
	}

	ops := NewOperations(store)
	rig := operationtest.NewRig(t, &operationtest.Options{
		DB:       db,
		Catalogs: []operation.Catalog{Catalog()},
		Bindings: func(*operationtest.Stubs) []operation.Binding { return ops.Bindings() },
	})
	registry := rig.Registry

	cliCaller := operation.Caller{Surface: operation.SurfaceCLI, PrincipalID: "local-owner"}
	preview, err := registry.Preview(ctx, "workspace.untaint", &operation.Call{
		Caller: cliCaller,
		Input:  json.RawMessage(`{"path":"a.txt"}`),
	})
	if err != nil {
		t.Fatalf("preview a.txt: %v", err)
	}
	if preview.Code == "" || preview.Digest == "" {
		t.Fatalf("preview = %+v, want confirmation code and digest", preview)
	}
	if !strings.Contains(strings.Join(preview.Lines, "\n"), "a.txt") {
		t.Errorf("preview lines = %v, want a.txt", preview.Lines)
	}

	if _, err := registry.Invoke(ctx, "workspace.untaint", &operation.Call{
		Caller:        cliCaller,
		Input:         json.RawMessage(`{"path":"a.txt"}`),
		PreviewDigest: preview.Digest,
		Confirmation:  "WRONG-CODE",
	}); err == nil {
		t.Fatal("wrong confirmation code succeeded")
	}

	if _, err := registry.Invoke(ctx, "workspace.untaint", &operation.Call{
		Caller:        cliCaller,
		Input:         json.RawMessage(`{"path":"a.txt"}`),
		PreviewDigest: preview.Digest,
		Confirmation:  preview.Code,
	}); err != nil {
		t.Fatalf("apply a.txt: %v", err)
	}
	remaining, err := store.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(remaining) != 1 || remaining[0].Path != "b.txt" {
		t.Fatalf("remaining after single-path untaint = %+v, want only b.txt", remaining)
	}

	if _, err := store.RecordWorkspaceWide(ctx, "exec", "sess-1"); err != nil {
		t.Fatalf("RecordWorkspaceWide: %v", err)
	}
	previewAll, err := registry.Preview(ctx, "workspace.untaint", &operation.Call{
		Caller: cliCaller,
		Input:  json.RawMessage(`{}`),
	})
	if err != nil || previewAll.Code == "" {
		t.Fatalf("preview all = %+v %v", previewAll, err)
	}
	if len(previewAll.Lines) < 2 || !strings.HasPrefix(previewAll.Lines[0], "workspace-wide") {
		t.Errorf("preview all lines = %v, want workspace-wide row listed first", previewAll.Lines)
	}
	if _, err := registry.Invoke(ctx, "workspace.untaint", &operation.Call{
		Caller:        cliCaller,
		Input:         json.RawMessage(`{}`),
		PreviewDigest: previewAll.Digest,
		Confirmation:  previewAll.Code,
	}); err != nil {
		t.Fatalf("apply all: %v", err)
	}
	has, err := store.HasRecords(ctx)
	if err != nil || has {
		t.Fatalf("HasRecords after untaint all = %v %v, want false", has, err)
	}

	t.Run("record mutation between preview and apply returns preview stale", func(t *testing.T) {
		if err := store.RecordFile(ctx, "c.txt", "3333333333333333333333333333333333333333333333333333333333333333", 1, 103, "web_fetch", "sess-2"); err != nil {
			t.Fatalf("RecordFile c.txt: %v", err)
		}
		stalePreview, err := registry.Preview(ctx, "workspace.untaint", &operation.Call{
			Caller: cliCaller,
			Input:  json.RawMessage(`{}`),
		})
		if err != nil {
			t.Fatalf("Preview: %v", err)
		}
		if err := store.RecordFile(ctx, "d.txt", "4444444444444444444444444444444444444444444444444444444444444444", 1, 104, "web_search", "sess-2"); err != nil {
			t.Fatalf("RecordFile d.txt: %v", err)
		}
		_, err = registry.Invoke(ctx, "workspace.untaint", &operation.Call{
			Caller:        cliCaller,
			Input:         json.RawMessage(`{}`),
			PreviewDigest: stalePreview.Digest,
			Confirmation:  stalePreview.Code,
		})
		if !fault.HasCode(err, operation.ErrorCodePreviewStale) {
			t.Fatalf("stale apply error = %v, want %s", err, operation.ErrorCodePreviewStale)
		}
	})
}
