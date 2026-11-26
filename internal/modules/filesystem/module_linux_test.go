//go:build linux

package filesystem

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	"github.com/anggasct/aura/internal/platform/storage"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

func TestModuleRegistersAndExecutesWorkspaceTools(t *testing.T) {
	workspace := t.TempDir()
	mod, err := New(&Deps{
		Workspace:     workspace,
		MaxFileBytes:  4096,
		MaxDirEntries: 100,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(mod.Tools()) != 3 {
		t.Fatalf("Tools() count = %d, want 3", len(mod.Tools()))
	}

	db := sqlitetest.OpenMigrated(t, turnstore.Schema(), effect.Schema(), storage.Schema())
	turns := turnstore.NewTurnStore(db)
	if err := turns.CreateSession(t.Context(), &turn.Session{ID: "sess-1", OwnerID: "owner"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	journal, err := effect.NewJournal(db, effect.Options{Logger: slog.New(slog.DiscardHandler), Turns: turns})
	if err != nil {
		t.Fatalf("effect.NewJournal: %v", err)
	}
	effects, err := effect.NewExecutor(journal)
	if err != nil {
		t.Fatalf("effect.NewExecutor: %v", err)
	}

	broker, err := toolbroker.New(&toolbroker.Options{
		Policy:           approval.Policy{Version: "builtin-tools-v1", Rules: map[string]approval.Rule{}},
		Effects:          effects,
		HostCapabilities: []string{"workspace-read", "workspace-write"},
		ApprovalDecider: func(_ context.Context, _ *toolbroker.ApprovalPrompt) (bool, error) {
			return true, nil
		},
	})
	if err != nil {
		t.Fatalf("toolbroker.New: %v", err)
	}
	if err := broker.RegisterProvider(mod); err != nil {
		t.Fatalf("RegisterProvider: %v", err)
	}

	writeResp, err := broker.Execute(t.Context(), &toolbroker.ToolRequest{
		RequestID:             "req-write",
		TurnID:                "turn-1",
		SessionID:             "sess-1",
		PrincipalID:           "owner",
		ToolName:              WriteFileToolName,
		ToolVersion:           "v1",
		Arguments:             json.RawMessage(`{"path":"hello.txt","content":"world"}`),
		Capabilities:          []string{"workspace-write"},
		Trust:                 approval.TrustOwnerInput,
		IdempotencyKey:        "idem-write-1",
		AllocateEventSequence: true,
	})
	if err != nil || writeResp.Class != toolbroker.ResultOK {
		t.Fatalf("write_file = %+v, %v", writeResp, err)
	}

	readResp, err := broker.Execute(t.Context(), &toolbroker.ToolRequest{
		RequestID:    "req-read",
		TurnID:       "turn-1",
		SessionID:    "sess-1",
		PrincipalID:  "owner",
		ToolName:     ReadFileToolName,
		ToolVersion:  "v1",
		Arguments:    json.RawMessage(`{"path":"hello.txt"}`),
		Capabilities: []string{"workspace-read"},
		Trust:        approval.TrustOwnerInput,
	})
	if err != nil || readResp.Class != toolbroker.ResultOK {
		t.Fatalf("read_file = %+v, %v", readResp, err)
	}
	var gotFile fileResult
	if err := json.Unmarshal(readResp.Output, &gotFile); err != nil || gotFile.Content != "world" {
		t.Fatalf("read_file output = %s, %v", readResp.Output, err)
	}

	listResp, err := broker.Execute(t.Context(), &toolbroker.ToolRequest{
		RequestID:    "req-list",
		TurnID:       "turn-1",
		SessionID:    "sess-1",
		PrincipalID:  "owner",
		ToolName:     ListDirToolName,
		ToolVersion:  "v1",
		Arguments:    json.RawMessage(`{"path":"."}`),
		Capabilities: []string{"workspace-read"},
		Trust:        approval.TrustOwnerInput,
	})
	if err != nil || listResp.Class != toolbroker.ResultOK {
		t.Fatalf("list_dir = %+v, %v", listResp, err)
	}
	var gotDir directoryResult
	if err := json.Unmarshal(listResp.Output, &gotDir); err != nil || len(gotDir.Entries) != 1 || gotDir.Entries[0].Path != "hello.txt" {
		t.Fatalf("list_dir output = %s, %v", listResp.Output, err)
	}
}

func TestModuleRejectsRelativeWorkspace(t *testing.T) {
	if _, err := New(&Deps{Workspace: "relative"}); err == nil {
		t.Fatal("New accepted relative workspace")
	}
}
