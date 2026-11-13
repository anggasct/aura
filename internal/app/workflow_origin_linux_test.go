package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	toolsbuiltin "github.com/anggasct/aura/internal/modules/builtin"
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

func TestWorkflowStartCarriesTheCallerTaint(t *testing.T) {
	tests := []struct {
		name    string
		tainted bool
		column  int
	}{
		{name: "untainted request", tainted: false, column: 0},
		{name: "tainted request", tainted: true, column: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			controller, runs, db := workflowToolFixture(t, "book_visit")
			cfg := mustLoadConfig(t, writePersonaEvalConfig(t, "x", "web_search"))
			approveAll := func(context.Context, *toolbroker.ApprovalPrompt) (bool, error) { return true, nil }
			executor, err := toolsbuiltin.New(cfg, db, t.TempDir(), capabilitytest.All(t), slog.New(slog.DiscardHandler), nil, approveAll, nil,
				toolsbuiltin.WithApprovalReachable(func(string) bool { return true }))
			if err != nil {
				t.Fatalf("toolsbuiltin.New: %v", err)
			}
			executor.SetWorkflowController(controller)
			result, err := executor.Broker().Execute(t.Context(), &toolbroker.ToolRequest{
				RequestID: "request-1", TurnID: "turn-1", SessionID: "lobby/terminal:owner", PrincipalID: principal.OwnerID,
				PrincipalClass: principal.ClassOwner, AgentID: "lobby", ToolName: "workflow_start", ToolVersion: "v1",
				Arguments: json.RawMessage(`{"id":"book_visit"}`), Trust: approval.TrustOwnerInput, IdempotencyKey: "idempotency-1",
				EventSequence: 1, Tainted: tc.tainted,
			})
			if err != nil {
				t.Fatalf("workflow_start: %v", err)
			}
			var started struct {
				RunID string `json:"run_id"`
			}
			if err := json.Unmarshal(result.Output, &started); err != nil || started.RunID == "" {
				t.Fatalf("workflow_start output = %s, %v", result.Output, err)
			}
			run, err := runs.Run(t.Context(), started.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if run.Origin.Tainted != tc.tainted {
				t.Fatalf("run tainted = %v, want %v", run.Origin.Tainted, tc.tainted)
			}
			if got := countRows(t, db, `SELECT COUNT(*) FROM workflow_run WHERE id = ? AND tainted = ?`, started.RunID, tc.column); got != 1 {
				t.Fatalf("stored taint column is not %d", tc.column)
			}
		})
	}
}
