//go:build linux

package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
)

func agentStepDefinition(id, persona, tools string) string {
	return fmt.Sprintf(`id: %s
version: 1
goal: Report on the first note
source: defined
steps:
  - id: report
    executor:
      kind: agent
      agent_id: %s
      prompt: Read first.txt and report what it says.
      tools: %s
    timeout: 1m
`, id, persona, tools)
}

type agentStepServer struct {
	srv  *compositionServer
	path string
	db   *sql.DB
	chat *agentChatServer
}

func newAgentStepServer(t *testing.T, definitions map[string]string, steps []agentScriptStep, capabilities ...string) *agentStepServer {
	t.Helper()
	restoreDefaultLogger(t)
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	dir := t.TempDir()
	for id, content := range definitions {
		if err := os.WriteFile(filepath.Join(dir, id+".yaml"), []byte(content), 0o600); err != nil {
			t.Fatalf("write definition %s: %v", id, err)
		}
	}
	chat := newAgentChatServer(t, steps)
	extra := fmt.Sprintf("workflows:\n  definitions_dir: %s\n", dir)
	path := writeCompositionConfig(t, freeLoopbackAddress(t), stubModelsYAML(chat.url), extra)
	workspace := loadCompositionConfig(t, path).Config.Tools.Workspace
	if err := os.WriteFile(filepath.Join(workspace, "first.txt"), []byte("the first note"), 0o600); err != nil {
		t.Fatalf("write first.txt: %v", err)
	}
	result := loadCompositionConfig(t, path)
	result.CapabilityReport = capabilitytest.Report(t, append([]string{"workspace-read"}, capabilities...)...)
	server := &agentStepServer{path: path, chat: chat}
	server.srv = &compositionServer{app: buildApp(t, serverCompositionOptions(result, &syncBuffer{}))}
	t.Cleanup(func() {
		if server.srv.ran != nil {
			server.srv.cancel()
			<-server.srv.ran
		}
		_ = server.srv.app.Close(context.Background())
	})
	server.srv.run(t)
	server.db = compositionDatabase(t, path)
	return server
}

type agentEnvelope struct {
	Text    string `json:"text"`
	Tainted bool   `json:"tainted"`
	Private bool   `json:"private"`
}

type stepRow struct {
	status string
	output agentEnvelope
	code   string
}

func readStepRow(t *testing.T, db *sql.DB, runID, stepID string) stepRow {
	t.Helper()
	var row stepRow
	var output, code sql.NullString
	if err := db.QueryRowContext(t.Context(), `SELECT status, output_json, error_code FROM workflow_step_run WHERE run_id = ? AND step_id = ?`, runID, stepID).Scan(&row.status, &output, &code); err != nil {
		t.Fatalf("read step %s of %s: %v", stepID, runID, err)
	}
	row.code = code.String
	if output.Valid && output.String != "" {
		if err := json.Unmarshal([]byte(output.String), &row.output); err != nil {
			t.Fatalf("decode the output of %s: %v", stepID, err)
		}
	}
	return row
}

func TestWorkflowAgentStepRunsOnTheServer(t *testing.T) {
	tests := []struct {
		name        string
		tools       string
		steps       []agentScriptStep
		wantOffered []string
		wantCalls   int
		wantPrivate bool
	}{
		{
			name:  "a workspace step reads a file and answers",
			tools: "[read_file]",
			steps: []agentScriptStep{
				{call: &genai.FunctionCall{Name: "read_file", Args: map[string]any{"path": "first.txt"}}},
				{text: "the note says: the first note"},
			},
			wantOffered: []string{"read_file"},
			wantCalls:   2,
			wantPrivate: true,
		},
		{
			name:        "a step without tools is offered none",
			tools:       "[]",
			steps:       []agentScriptStep{{text: "the note says: the first note"}},
			wantOffered: []string{},
			wantCalls:   1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := newAgentStepServer(t, map[string]string{"note-agent": agentStepDefinition("note-agent", "reviewer", tc.tools)}, tc.steps)
			summary, err := server.srv.app.interpreter.StartConfigured(t.Context(), &workflow.ConfiguredStart{DefinitionID: "note-agent", Input: nil, PrincipalID: principal.OwnerID, Class: principal.ClassOwner, DurableKey: ""})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if got, want := awaitRunEnded(t, server.db, summary.DurableKey), (storedRun{handler: workflow.HandlerName, state: "succeeded"}); got != want {
				t.Fatalf("workflow run = %+v, want %+v", got, want)
			}
			row := readStepRow(t, server.db, summary.ID, "report")
			if row.status != string(workflow.StepSucceeded) || row.output.Text != "the note says: the first note" || row.output.Tainted || row.output.Private != tc.wantPrivate {
				t.Fatalf("step row = %+v, want the model's final text, untainted, private %v", row, tc.wantPrivate)
			}
			requests := server.chat.log.seen()
			if len(requests) != tc.wantCalls {
				t.Fatalf("model calls = %d, want %d", len(requests), tc.wantCalls)
			}
			for i, request := range requests {
				if !slices.Equal(request.Tools, tc.wantOffered) {
					t.Errorf("model request %d offered %v, want exactly %v", i+1, request.Tools, tc.wantOffered)
				}
			}
			if tc.wantCalls == 2 && (len(requests[1].Results) != 1 || !strings.Contains(requests[1].Results[0], "the first note")) {
				t.Errorf("the second model request carries %v, want the read_file result", requests[1].Results)
			}
			sessionID := workflow.StepSessionID(summary.ID, "report", 0)
			if n := countRows(t, server.db, `SELECT COUNT(*) FROM session WHERE id = ? AND owner_id = ? AND agent_id = ?`, sessionID, principal.OwnerID, "reviewer"); n != 1 {
				t.Errorf("step sessions %s owned by the origin and served by the step persona = %d, want 1", sessionID, n)
			}
			if n := countRows(t, server.db, `SELECT COUNT(*) FROM runtime_event WHERE session_id = ?`, sessionID); n == 0 {
				t.Errorf("the step session %s holds no event", sessionID)
			}
			if n := countRows(t, server.db, `SELECT COUNT(*) FROM usage_reservation WHERE state = 'settled' AND agent_id = ? AND principal_id = ? AND principal_class = ?`, "reviewer", principal.OwnerID, string(principal.ClassOwner)); n != tc.wantCalls {
				t.Errorf("settled reservations of the step persona, origin principal and class = %d, want one per model call (%d)", n, tc.wantCalls)
			}
			server.srv.stopRun(t)
		})
	}
}

func TestTaintedAgentStepWebCallIsRefusedAndTheStepContinues(t *testing.T) {
	server := newAgentStepServer(t,
		map[string]string{"web-agent": agentStepDefinition("web-agent", "researcher", "[web_fetch]")},
		[]agentScriptStep{
			{call: &genai.FunctionCall{Name: "web_fetch", Args: map[string]any{"url": "https://example.com/"}}},
			{text: "the page could not be fetched"},
		},
		"public-web",
	)
	origin := &workflow.RunOrigin{PrincipalID: principal.OwnerID, PrincipalClass: string(principal.ClassOwner), AgentID: "main", Tainted: true}
	summary, err := server.srv.app.interpreter.Start(t.Context(), "web-agent", nil, origin)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got, want := awaitRunEnded(t, server.db, summary.DurableKey), (storedRun{handler: workflow.HandlerName, state: "succeeded"}); got != want {
		t.Fatalf("workflow run = %+v, want %+v", got, want)
	}
	requests := server.chat.log.seen()
	if len(requests) != 2 {
		t.Fatalf("model calls = %d, want 2: the refusal goes back to the model and the step continues", len(requests))
	}
	if !slices.Equal(requests[0].Tools, []string{"web_fetch"}) {
		t.Errorf("tools offered = %v, want exactly [web_fetch]", requests[0].Tools)
	}
	if len(requests[1].Results) != 1 || !strings.Contains(requests[1].Results[0], "untrusted_input_no_approver") {
		t.Errorf("the second model request carries %v, want the untrusted_input_no_approver refusal", requests[1].Results)
	}
	row := readStepRow(t, server.db, summary.ID, "report")
	if row.status != string(workflow.StepSucceeded) || row.output.Text != "the page could not be fetched" || !row.output.Tainted {
		t.Errorf("step row = %+v, want the model's next answer on a tainted output", row)
	}
	server.srv.stopRun(t)
}

func TestWorkflowStartFromCLIWithAgentStepRunsOnTheServer(t *testing.T) {
	server := newAgentStepServer(t,
		map[string]string{"note-agent": agentStepDefinition("note-agent", "reviewer", "[read_file]")},
		[]agentScriptStep{
			{call: &genai.FunctionCall{Name: "read_file", Args: map[string]any{"path": "first.txt"}}},
			{text: "the note says: the first note"},
		},
	)
	command := buildApp(t, commandCompositionOptions(loadCompositionConfig(t, server.path), &syncBuffer{}, PartWorkflows))
	t.Cleanup(func() { _ = command.Close(context.Background()) })
	summary, err := command.interpreter.StartConfigured(t.Context(), &workflow.ConfiguredStart{DefinitionID: "note-agent", Input: nil, PrincipalID: principal.OwnerID, Class: principal.ClassOwner, DurableKey: ""})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got, want := awaitRunEnded(t, server.db, summary.DurableKey), (storedRun{handler: workflow.HandlerName, state: "succeeded"}); got != want {
		t.Fatalf("workflow run = %+v, want %+v", got, want)
	}
	if row := readStepRow(t, server.db, summary.ID, "report"); row.status != string(workflow.StepSucceeded) || row.output.Text != "the note says: the first note" {
		t.Fatalf("step row = %+v, want the server's agent turn output", row)
	}
	server.srv.stopRun(t)
}

func TestCommandSurfaceBuildsNoAgentStepExecutor(t *testing.T) {
	restoreDefaultLogger(t)
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note-agent.yaml"), []byte(agentStepDefinition("note-agent", "reviewer", "[read_file]")), 0o600); err != nil {
		t.Fatalf("write definition: %v", err)
	}
	path := writeCompositionConfig(t, freeLoopbackAddress(t), compositionModelsYAML, fmt.Sprintf("workflows:\n  definitions_dir: %s\n", dir))
	command := newBuild(commandCompositionOptions(loadCompositionConfig(t, path), &syncBuffer{}, PartWorkflows))
	for _, step := range command.phases(t.Context()) {
		if err := step(); err != nil {
			t.Fatalf("build phase: %v", err)
		}
	}
	if err := command.workflowMod.BindAgentTurns(nil, nil); !fault.HasCode(err, workflow.ErrorCodeInvalidArgument) {
		t.Fatalf("BindAgentTurns on a command surface module = %v, want %s", err, workflow.ErrorCodeInvalidArgument)
	}
	summary, err := command.interpreter.StartConfigured(t.Context(), &workflow.ConfiguredStart{DefinitionID: "note-agent", Input: nil, PrincipalID: principal.OwnerID, Class: principal.ClassOwner, DurableKey: ""})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	db := compositionDatabase(t, path)
	if n := countRows(t, db, `SELECT COUNT(*) FROM durable_run WHERE key = ? AND state = 'running' AND claim = 0 AND owner IS NULL`, summary.DurableKey); n != 1 {
		t.Fatalf("unclaimed durable runs %s = %d, want 1: a command surface never drives a run", summary.DurableKey, n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM workflow_step_run WHERE run_id = ? AND status <> 'pending'`, summary.ID); n != 0 {
		t.Fatalf("started or ended step rows of %s = %d, want 0", summary.ID, n)
	}
}

func TestServerBuildRefusesAnUnboundAgentStepExecutor(t *testing.T) {
	b := buildServerPhases(t, "")
	if err := b.buildWorkflows(t.Context(), false); err != nil {
		t.Fatalf("buildWorkflows without a host: %v", err)
	}
	before := b.group.Names()
	err := b.wireWorkflowParts()
	if !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("wireWorkflowParts = %v, want %s", err, ErrorCodeInvalidArgument)
	}
	if after := b.group.Names(); len(after) != len(before) {
		t.Fatalf("lifecycle entries %v changed to %v", before, after)
	}
}
