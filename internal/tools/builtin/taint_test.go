//go:build linux

package toolsbuiltin

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/capability/capabilitytest"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/runtime/adk"
	"github.com/anggasct/aura/internal/store/storetest"
	"github.com/anggasct/aura/internal/toolbroker"
)

func TestExecutorForwardsTheTaintFlagToTheBroker(t *testing.T) {
	db := storetest.OpenMigrated(t)
	cfg := config.Default()
	cfg.Tools.Workspace = t.TempDir()
	cfg.Storage.Path = t.TempDir()
	decisions := 0
	executor, err := New(&cfg, db, cfg.Storage.Path, capabilitytest.All(t), nil, nil, func(context.Context, *toolbroker.ApprovalPrompt) (bool, error) {
		decisions++
		return false, nil
	}, nil, WithApprovalReachable(func(sessionID string) bool { return sessionID == "reachable" }))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	request := func(session string, tainted bool) *runtimeadk.BuiltinToolRequest {
		return &runtimeadk.BuiltinToolRequest{
			RequestID: "call-1", TurnID: "turn-1", SessionID: session, PrincipalID: "owner-1",
			ToolName: "web_search", ToolVersion: "v1", Arguments: json.RawMessage(`{"query":"x"}`),
			Capabilities: []string{"provider-search"}, Trust: "derived_untrusted", Deadline: time.Now().Add(time.Minute),
			IdempotencyKey: "k", Tainted: tainted, Knowledge: []string{"faq"},
		}
	}
	_, err = executor.Execute(t.Context(), request("unreachable", true))
	if class, _ := toolbroker.CodeOf(err); class != toolbroker.ResultKnowledgeSinkNoApprover || decisions != 0 {
		t.Fatalf("tainted on an unreachable session: %v, decisions %d", err, decisions)
	}
	_, err = executor.Execute(t.Context(), request("reachable", true))
	if class, _ := toolbroker.CodeOf(err); class != toolbroker.ResultApprovalRequired || decisions != 1 {
		t.Fatalf("tainted on a reachable session: %v, decisions %d, want a rejected prompt", err, decisions)
	}
	_, err = executor.Execute(t.Context(), request("unreachable", false))
	if class, _ := toolbroker.CodeOf(err); class == toolbroker.ResultKnowledgeSinkNoApprover || decisions != 1 {
		t.Fatalf("untainted call: %v, decisions %d, want no taint denial and no prompt", err, decisions)
	}
}
