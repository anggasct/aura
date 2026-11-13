//go:build linux

package toolsbuiltin

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/toolbroker/toolbrokertest"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeadk "github.com/anggasct/aura/internal/platform/turn/adk"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/store/storetest"
)

func TestTurnToolCallsLetTheJournalAllocateTheSequence(t *testing.T) {
	ctx := t.Context()
	dataRoot := t.TempDir()
	db := storetest.OpenMigrated(t)
	const session = "session-allocate"
	if err := store.NewSessionService(db).Create(ctx, &store.Session{ID: session, OwnerID: principal.OwnerID}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	cfg := config.Default()
	cfg.Tools.Workspace = t.TempDir()
	cfg.Storage.Path = dataRoot
	executor, err := New(&cfg, db, dataRoot, capabilitytest.All(t), nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var seen []toolbroker.ToolRequest
	toolbrokertest.Register(t, executor.Broker(), toolbrokertest.Stub{
		Name: "stub_effect", Effectful: true,
		Run: func(_ context.Context, req *toolbroker.ToolRequest, _ approval.Constraints) (toolbroker.ToolResult, error) {
			seen = append(seen, *req)
			return toolbroker.ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
		},
	})
	request := &runtimeadk.BuiltinToolRequest{
		RequestID: "call-allocate", TurnID: "turn-allocate", SessionID: session,
		PrincipalID: principal.OwnerID, PrincipalClass: principal.ClassOwner,
		ToolName: "stub_effect", ToolVersion: "v1", Arguments: json.RawMessage(`{"n":1}`),
		Trust: string(approval.TrustDerivedUntrusted), Deadline: time.Now().Add(time.Minute),
		IdempotencyKey: "adk/" + session + "/turn-allocate/call-allocate", EventInvocation: "inv-allocate",
		EventBranch: "main", EventAuthor: "agent",
	}
	events := store.NewEventStore(db)
	for sequence := uint64(1); sequence <= 2; sequence++ {
		if err := events.Append(ctx, &store.RuntimeEvent{
			ID: "other-" + string(rune('0'+sequence)), SessionID: session, Sequence: sequence, TurnID: "turn-other",
			Kind: turn.EventKindTurnAccepted, SchemaVersion: 1, Payload: json.RawMessage(`{}`), CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("append the other turn's event %d: %v", sequence, err)
		}
	}
	if _, err := executor.Execute(ctx, request); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(seen) != 1 || !seen[0].AllocateEventSequence || seen[0].EventSequence != 0 {
		t.Fatalf("broker requests = %+v, want one that lets the journal allocate its sequence", seen)
	}
	var sequence uint64
	if err := db.QueryRowContext(ctx, `SELECT sequence FROM runtime_event WHERE session_id = ? AND kind = ?`, session, turn.EventKindToolRequested).Scan(&sequence); err != nil {
		t.Fatalf("read the tool.requested event: %v", err)
	}
	if sequence != 3 {
		t.Fatalf("tool.requested sequence = %d, want 3, the session's next free sequence", sequence)
	}
}
