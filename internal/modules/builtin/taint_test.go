//go:build linux

package toolsbuiltin

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/modules/search"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/secret/secrettest"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/turn/adk"
	"github.com/anggasct/aura/internal/store/storetest"
)

func TestExecutorForwardsTheTaintFlagToTheBroker(t *testing.T) {
	db := storetest.OpenMigrated(t)
	toolsCfg := config.DefaultTools()
	toolsCfg.WebSearch = search.Config{Provider: "brave", CredentialRef: secrettest.Ref("env://AURA_TEST_TAINT_SEARCH_UNSET"), MaxResults: 5}
	toolsCfg.Workspace = t.TempDir()
	dataRoot := t.TempDir()
	decisions := 0
	executor, err := New(toolsCfg, testArtifactQuota, db, dataRoot, capabilitytest.All(t), nil, nil, func(context.Context, *toolbroker.ApprovalPrompt) (bool, error) {
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
	if class, _ := fault.CodeOf[toolbroker.ResultClass](err); class != toolbroker.ResultUntrustedInputNoApprover || decisions != 0 {
		t.Fatalf("tainted on an unreachable session: %v, decisions %d", err, decisions)
	}
	_, err = executor.Execute(t.Context(), request("reachable", true))
	if class, _ := fault.CodeOf[toolbroker.ResultClass](err); class != toolbroker.ResultApprovalRequired || decisions != 1 {
		t.Fatalf("tainted on a reachable session: %v, decisions %d, want a rejected prompt", err, decisions)
	}
	_, err = executor.Execute(t.Context(), request("unreachable", false))
	if class, _ := fault.CodeOf[toolbroker.ResultClass](err); class == toolbroker.ResultUntrustedInputNoApprover || decisions != 1 {
		t.Fatalf("untainted call: %v, decisions %d, want no taint denial and no prompt", err, decisions)
	}
}
