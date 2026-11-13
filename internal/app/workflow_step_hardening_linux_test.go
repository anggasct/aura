package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	toolsbuiltin "github.com/anggasct/aura/internal/modules/builtin"
	"github.com/anggasct/aura/internal/modules/workflow"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/store"
)

type countingWorkflowController struct{ calls atomic.Int64 }

func (c *countingWorkflowController) SaveRoutine(context.Context, *toolsbuiltin.WorkflowCaller, string, string) (toolsbuiltin.WorkflowRoutine, error) {
	c.calls.Add(1)
	return toolsbuiltin.WorkflowRoutine{}, nil
}

func (c *countingWorkflowController) Propose(context.Context, *toolsbuiltin.WorkflowCaller, json.RawMessage) (toolsbuiltin.WorkflowProposal, error) {
	c.calls.Add(1)
	return toolsbuiltin.WorkflowProposal{}, nil
}

func (c *countingWorkflowController) List(context.Context, *toolsbuiltin.WorkflowCaller) ([]toolsbuiltin.WorkflowInfo, error) {
	c.calls.Add(1)
	return nil, nil
}

func (c *countingWorkflowController) Start(context.Context, *toolsbuiltin.WorkflowCaller, string, string) (toolsbuiltin.WorkflowRunState, error) {
	c.calls.Add(1)
	return toolsbuiltin.WorkflowRunState{}, nil
}

func (c *countingWorkflowController) Status(context.Context, *toolsbuiltin.WorkflowCaller, string) (toolsbuiltin.WorkflowRunState, error) {
	c.calls.Add(1)
	return toolsbuiltin.WorkflowRunState{}, nil
}

func (c *countingWorkflowController) Cancel(context.Context, *toolsbuiltin.WorkflowCaller, string) (toolsbuiltin.WorkflowRunState, error) {
	c.calls.Add(1)
	return toolsbuiltin.WorkflowRunState{}, nil
}

type countingHandoffOpener struct{ calls atomic.Int64 }

func (o *countingHandoffOpener) OpenHandoff(context.Context, *toolsbuiltin.HandoffRequest) error {
	o.calls.Add(1)
	return nil
}

func TestWorkflowToolRunnerRefusesAnOrchestrationTool(t *testing.T) {
	runner, _, _ := newWorkflowToolRunnerFixture(t)
	toolRunner, ok := runner.(*workflowToolRunner)
	if !ok {
		t.Fatalf("runner is %T, want *workflowToolRunner", runner)
	}
	controller, opener := &countingWorkflowController{}, &countingHandoffOpener{}
	toolRunner.executor.SetWorkflowController(controller)
	toolRunner.executor.SetHandoffOpener(opener)

	for name, args := range map[string]json.RawMessage{
		"workflow_status":  json.RawMessage(`{"run_id":"r"}`),
		"handoff_to_owner": json.RawMessage(`{"reason":"x"}`),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := runner.Invoke(stepCallContext(t), name, args)
			if code, ok := workflow.CodeOf(err); !ok || code != workflow.ErrorCodeExecutorInvalid {
				t.Fatalf("Invoke error = %v, want %s", err, workflow.ErrorCodeExecutorInvalid)
			}
		})
	}
	if controller.calls.Load() != 0 || opener.calls.Load() != 0 {
		t.Fatalf("orchestration adapters ran: controller calls = %d, handoff calls = %d", controller.calls.Load(), opener.calls.Load())
	}
}

func TestStepToolCatalogMatchesTheBroker(t *testing.T) {
	var configured config.Config
	runner, _, _ := newWorkflowToolRunnerFixtureWith(t, func(cfg *config.Config) {
		cfg.Tools.GitHub.Credentials = []config.GitHubCredential{{Name: "ci", Ref: "env://AURA_TEST_GITHUB_TOKEN"}}
		configured = *cfg
	})
	toolRunner, ok := runner.(*workflowToolRunner)
	if !ok {
		t.Fatalf("runner is %T, want *workflowToolRunner", runner)
	}
	broker := toolRunner.executor.Broker()
	catalog, err := stepToolCatalog(&configured)
	if err != nil {
		t.Fatalf("stepToolCatalog: %v", err)
	}
	comment := func(credential string) json.RawMessage {
		return json.RawMessage(`{"repo":"org/repo","number":7,"body":"hi","credential_name":"` + credential + `"}`)
	}
	tests := []struct {
		name string
		tool string
		args json.RawMessage
	}{
		{name: "a valid read_file", tool: "read_file", args: json.RawMessage(`{"path":"a.txt"}`)},
		{name: "a schema violation", tool: "read_file", args: json.RawMessage(`{"path":""}`)},
		{name: "an unknown credential", tool: "github_comment", args: comment("nope")},
		{name: "a configured credential", tool: "github_comment", args: comment("ci")},
		{name: "an unknown tool", tool: "no_such_tool", args: json.RawMessage(`{"path":"a"}`)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			catalogErr := catalog.CheckArguments(tc.tool, tc.args)
			_, brokerErr := broker.ExactCallText(tc.tool, tc.args)
			catalogClass, _ := toolbroker.CodeOf(catalogErr)
			brokerClass, _ := toolbroker.CodeOf(brokerErr)
			if (catalogErr == nil) != (brokerErr == nil) || catalogClass != brokerClass {
				t.Fatalf("catalog = %v (%s), broker = %v (%s)", catalogErr, catalogClass, brokerErr, brokerClass)
			}
		})
	}
}

func commentWorkflow(credential string) *workflow.Spec {
	tool := "github_comment"
	spec := toolStepSpec(tool)
	spec.Steps[1].Executor.ToolArgs = json.RawMessage(`{"repo":"org/repo","number":7,"body":"hi","credential_name":"` + credential + `"}`)
	return spec
}

func TestWorkflowValidateRefusesAnUnconfiguredCredentialName(t *testing.T) {
	t.Run("the deps every compile path shares", func(t *testing.T) {
		configured := config.Default()
		configured.Tools.GitHub.Credentials = []config.GitHubCredential{{Name: "ci", Ref: "env://AURA_TEST_GITHUB_TOKEN"}}
		deps, err := workflowValidationDeps(&configured)
		if err != nil {
			t.Fatalf("workflowValidationDeps: %v", err)
		}
		_, err = workflow.Compile(commentWorkflow("nope"), deps)
		if code, ok := workflow.CodeOf(err); !ok || code != workflow.ErrorCodeSpecInvalid {
			t.Fatalf("Compile with an unconfigured credential = %v, want %s", err, workflow.ErrorCodeSpecInvalid)
		}
		if err == nil || !strings.Contains(err.Error(), `"call"`) {
			t.Fatalf("Compile error does not name the step: %v", err)
		}
		if _, err := workflow.Compile(commentWorkflow("ci"), deps); err != nil {
			t.Fatalf("Compile with a configured credential: %v", err)
		}
	})

	t.Run("aura workflow validate", func(t *testing.T) {
		gf := writeWorkflowFixtures(t, "")
		file := filepath.Join(t.TempDir(), "read.yaml")
		body := "id: read\nversion: 1\ngoal: Read a file\nsource: defined\nsteps:\n" +
			"  - id: load\n    timeout: 1m\n    executor:\n      kind: tool\n      tool: read_file\n      args: { path: \"\" }\n"
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, err := runWorkflowCommand(t, gf, "validate", file)
		if code, ok := workflow.CodeOf(err); !ok || code != workflow.ErrorCodeSpecInvalid {
			t.Fatalf("validate of invalid arguments = %v, want %s", err, workflow.ErrorCodeSpecInvalid)
		}
		if err == nil || !strings.Contains(err.Error(), `"load"`) {
			t.Fatalf("validate error does not name the step: %v", err)
		}
	})
}

func TestProposalApprovalLoadStaysStrict(t *testing.T) {
	h := proposalFixture(t)
	proposed, err := h.controller.Propose(t.Context(), h.owner, json.RawMessage(coveredProposal))
	if err != nil {
		t.Fatal(err)
	}
	refusing := h.controller.interpreter.ValidationDeps()
	refusing.CheckToolCall = func(string, json.RawMessage) error { return errors.New("a credential was removed") }
	if err := h.controller.interpreter.LoadAll(t.Context(), nil, refusing); err != nil {
		t.Fatal(err)
	}
	_, err = approveProposal(t.Context(), h.cfg, h.controller.interpreter, h.runs, proposed.ProposalID, h.controller.showCall, time.Now())
	if !hasWorkflowCode(err, workflow.ErrorCodeSpecInvalid) {
		t.Fatalf("approval of a proposal that fails the argument check = %v, want %s", err, workflow.ErrorCodeSpecInvalid)
	}
	stored, err := h.runs.Proposal(t.Context(), proposed.ProposalID, time.Now())
	if err != nil || stored.Status != workflow.ProposalProposed || stored.RunID != "" {
		t.Fatalf("proposal after the refused approval = %+v, %v", stored, err)
	}
	if _, err := h.runs.Approval(t.Context(), stored.Spec.ID, stored.Spec.Version); err == nil {
		t.Fatal("a refused approval left an approval record")
	}
	runs, err := h.runs.Runs(t.Context(), "")
	if err != nil || len(runs) != 0 {
		t.Fatalf("runs after the refused approval = %+v, %v", runs, err)
	}
}

func quarantineRoutine(id string) *workflow.Spec {
	tool := "read_file"
	return &workflow.Spec{
		ID: id, Name: id, Goal: "Read a file", Version: 1, Source: workflow.SourceGenerated,
		Steps: []workflow.StepSpec{{
			ID: "read", Timeout: time.Minute,
			Executor: workflow.ExecutorSpec{Kind: workflow.KindTool, ToolID: &tool, ToolArgs: json.RawMessage(`{"path":""}`)},
		}},
	}
}

func seedRoutine(t *testing.T, db *sql.DB, spec *workflow.Spec) {
	t.Helper()
	encoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO workflow_definition (id, version, goal, source, spec_json, spec_sha256, created_at, name) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		spec.ID, spec.Version, spec.Goal, string(spec.Source), string(encoded), "sha-"+spec.ID, time.Now().UTC().Format(time.RFC3339Nano), spec.Name); err != nil {
		t.Fatalf("seed routine: %v", err)
	}
}

func serverConfigWithRoutinePersona(t *testing.T, listed string) *config.LoadResult {
	t.Helper()
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	extra := fmt.Sprintf("durable:\n  enabled: true\n  mode: external\n  endpoint: http://%s\n  admin_endpoint: http://%s\n  handler_addr: %s\n",
		freeLoopbackAddress(t), freeLoopbackAddress(t), freeLoopbackAddress(t)) +
		"agents:\n  definitions:\n    - id: desk\n      description: trusted desk\n      instructions: \"You answer trusted callers.\"\n      audience: trusted\n      tools: [read_file]\n      principal_tools: [read_file]\n      workflows: [" + listed + "]\n" +
		"access:\n  budgets:\n    trusted:\n      max_tokens_per_day: 100000\n      max_turns_per_minute: 50\n"
	path := writeCompositionConfig(t, freeLoopbackAddress(t), compositionModelsYAML, extra)
	seedConfiguredStorage(t, path)
	result := loadCompositionConfig(t, path)
	dbPath, _, _, err := storagePaths(result.Config)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.OpenDB(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	seedRoutine(t, db, quarantineRoutine("broken_routine"))
	return result
}

func TestServerReportsQuarantinedWorkflowDefinitions(t *testing.T) {
	restoreDefaultLogger(t)

	t.Run("a persona naming a quarantined routine still boots", func(t *testing.T) {
		result := serverConfigWithRoutinePersona(t, "broken_routine")
		b := newBuild(serverCompositionOptions(result, &syncBuffer{}))
		for _, step := range b.phases(t.Context()) {
			if err := step(); err != nil {
				_ = b.group.Stop(t.Context())
				t.Fatalf("boot: %v", err)
			}
		}
		t.Cleanup(func() { _ = b.group.Stop(context.WithoutCancel(t.Context())) })
		if _, ok := b.interpreter.Spec("broken_routine"); ok {
			t.Fatal("the quarantined routine is loaded")
		}
		var check *health.RegisteredCheck
		for i := range b.server.checks {
			if b.server.checks[i].ID == workflowDefinitionsComponent {
				check = &b.server.checks[i]
			}
		}
		if check == nil {
			t.Fatal("no workflow_definitions health check is registered")
		}
		findings := check.Checker.Check(t.Context())
		if len(findings) != 1 || findings[0].Status != health.StatusDegraded {
			t.Fatalf("findings = %+v, want one degraded finding", findings)
		}
		for _, want := range []string{"broken_routine", "desk"} {
			if !strings.Contains(findings[0].Detail, want) {
				t.Fatalf("finding detail %q does not name %q", findings[0].Detail, want)
			}
		}
	})

	t.Run("a persona naming an id that never existed still fails boot", func(t *testing.T) {
		result := serverConfigWithRoutinePersona(t, "never_existed")
		app, err := Build(t.Context(), serverCompositionOptions(result, &syncBuffer{}))
		if app != nil {
			_ = app.Close(t.Context())
			t.Fatal("Build accepted a persona that lists an unknown workflow")
		}
		if code, ok := config.CodeOf(err); !ok || code != config.ErrorCodePersonaInvalid {
			t.Fatalf("Build error = %v, want %s", err, config.ErrorCodePersonaInvalid)
		}
	})
}

func hasConfigCode(err error, want config.ErrorCode) bool {
	code, ok := config.CodeOf(err)
	return ok && code == want
}

func TestQuarantinedRoutineIsHiddenFromTheControllerTools(t *testing.T) {
	controller, _, _ := workflowToolFixture(t, "book_visit")
	registry, err := auraagent.Build([]config.AgentDefinition{{
		ID: "desk", Description: "desk", Instructions: "You help.", Audience: config.AudienceTrusted,
		Tools: []string{"workflow_start"}, PrincipalTools: []string{"workflow_start"}, Workflows: []string{"book_visit", "broken_routine"},
	}}, toolsbuiltin.DefinitionNames(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newWorkflowToolController(controller.interpreter, controller.runs, registry, nil); !hasConfigCode(err, config.ErrorCodePersonaInvalid) {
		t.Fatalf("controller without the quarantine list = %v, want %s", err, config.ErrorCodePersonaInvalid)
	}
	quarantined, err := newWorkflowToolController(controller.interpreter, controller.runs, registry, []string{"broken_routine"})
	if err != nil {
		t.Fatalf("controller with the quarantine list: %v", err)
	}
	trusted := caller(principal.ClassTrusted, "discord:111")
	trusted.AgentID = "desk"
	if got := listedIDs(t, quarantined, trusted); got != "book_visit" {
		t.Fatalf("workflow_list = %q, want only the loaded routine", got)
	}
	_, err = quarantined.Start(t.Context(), trusted, "broken_routine", "go")
	if err == nil {
		t.Fatal("workflow_start of a quarantined routine succeeded")
	}
}
