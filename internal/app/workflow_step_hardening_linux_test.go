package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/modules/github"
	"github.com/anggasct/aura/internal/modules/handoff"
	"github.com/anggasct/aura/internal/modules/workflow"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/secret/secrettest"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

type countingWorkflowController struct{ calls atomic.Int64 }

func (c *countingWorkflowController) SaveRoutine(context.Context, *workflow.Caller, string, string) (workflow.RoutineSummary, error) {
	c.calls.Add(1)
	return workflow.RoutineSummary{}, nil
}

func (c *countingWorkflowController) Propose(context.Context, *workflow.Caller, json.RawMessage) (workflow.ProposalSummary, error) {
	c.calls.Add(1)
	return workflow.ProposalSummary{}, nil
}

func (c *countingWorkflowController) List(context.Context, *workflow.Caller) ([]workflow.Info, error) {
	c.calls.Add(1)
	return nil, nil
}

func (c *countingWorkflowController) Start(context.Context, *workflow.Caller, string, string) (workflow.RunState, error) {
	c.calls.Add(1)
	return workflow.RunState{}, nil
}

func (c *countingWorkflowController) Status(context.Context, *workflow.Caller, string) (workflow.RunState, error) {
	c.calls.Add(1)
	return workflow.RunState{}, nil
}

func (c *countingWorkflowController) Cancel(context.Context, *workflow.Caller, string) (workflow.RunState, error) {
	c.calls.Add(1)
	return workflow.RunState{}, nil
}

type countingHandoffOpener struct{ calls atomic.Int64 }

func (o *countingHandoffOpener) OpenHandoff(context.Context, *handoff.Request) error {
	o.calls.Add(1)
	return nil
}

func TestWorkflowToolRunnerRefusesAnOrchestrationTool(t *testing.T) {
	runner, db, _ := newWorkflowToolRunnerFixture(t)
	toolRunner, ok := runner.(*workflow.ToolStepRunner)
	if !ok {
		t.Fatalf("runner is %T, want *workflow.ToolStepRunner", runner)
	}
	broker, ok := toolRunner.Broker().(*toolbroker.Broker)
	if !ok {
		t.Fatalf("broker is %T, want *toolbroker.Broker", toolRunner.Broker())
	}
	controller, opener := &countingWorkflowController{}, &countingHandoffOpener{}
	wfProvider, err := workflow.NewToolProvider(controller)
	if err != nil {
		t.Fatalf("NewToolProvider(workflow): %v", err)
	}
	if err := broker.RegisterProvider(wfProvider); err != nil {
		t.Fatalf("RegisterProvider(workflow): %v", err)
	}
	if err := broker.RegisterProvider(handoff.NewToolProvider(opener)); err != nil {
		t.Fatalf("RegisterProvider(handoff): %v", err)
	}

	for name, args := range map[string]json.RawMessage{
		"workflow_status":  json.RawMessage(`{"run_id":"r"}`),
		"handoff_to_owner": json.RawMessage(`{"reason":"x"}`),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := runner.Invoke(stepCallContext(t, db), name, args)
			if !fault.HasCode(err, workflow.ErrorCodeExecutorInvalid) {
				t.Fatalf("Invoke error = %v, want %s", err, workflow.ErrorCodeExecutorInvalid)
			}
		})
	}
	if controller.calls.Load() != 0 || opener.calls.Load() != 0 {
		t.Fatalf("orchestration adapters ran: controller calls = %d, handoff calls = %d", controller.calls.Load(), opener.calls.Load())
	}
}

func TestStepToolCatalogMatchesTheBroker(t *testing.T) {
	var configured appconfig.Config
	runner, _, _ := newWorkflowToolRunnerFixtureWith(t, func(cfg *appconfig.Config) {
		cfg.Tools.GitHub.Credentials = []github.Credential{{Name: "ci", Ref: secrettest.Ref("env://AURA_TEST_GITHUB_TOKEN")}}
		configured = *cfg
	})
	toolRunner, ok := runner.(*workflow.ToolStepRunner)
	if !ok {
		t.Fatalf("runner is %T, want *workflow.ToolStepRunner", runner)
	}
	broker := toolRunner.Broker()
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
			catalogClass, _ := fault.CodeOf[toolbroker.ResultClass](catalogErr)
			brokerClass, _ := fault.CodeOf[toolbroker.ResultClass](brokerErr)
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
		configured := appconfig.Default()
		configured.Tools.GitHub.Credentials = []github.Credential{{Name: "ci", Ref: secrettest.Ref("env://AURA_TEST_GITHUB_TOKEN")}}
		deps, err := workflowValidationDeps(&configured)
		if err != nil {
			t.Fatalf("workflowValidationDeps: %v", err)
		}
		_, err = workflow.Compile(commentWorkflow("nope"), deps)
		if !fault.HasCode(err, workflow.ErrorCodeSpecInvalid) {
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
		if !fault.HasCode(err, workflow.ErrorCodeSpecInvalid) {
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
	refusing := h.controller.Interpreter().ValidationDeps()
	refusing.CheckToolCall = func(string, json.RawMessage) error { return errors.New("a credential was removed") }
	if err := h.controller.Interpreter().LoadAll(t.Context(), nil, refusing); err != nil {
		t.Fatal(err)
	}
	_, err = workflow.ApproveRenderedProposal(t.Context(), h.controller.Interpreter(), h.runs, proposed.ProposalID, h.controller.ShowCall(), time.Now())
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

func serverConfigWithRoutinePersona(t *testing.T, listed string) *appconfig.LoadResult {
	t.Helper()
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	extra := "agents:\n  definitions:\n    - id: desk\n      description: trusted desk\n      instructions: \"You answer trusted callers.\"\n      audience: trusted\n      tools: [read_file]\n      principal_tools: [read_file]\n      workflows: [" + listed + "]\n" +
		"access:\n  budgets:\n    trusted:\n      max_tokens_per_day: 100000\n      max_turns_per_minute: 50\n"
	path := writeCompositionConfig(t, freeLoopbackAddress(t), compositionModelsYAML, extra)
	seedConfiguredStorage(t, path)
	result := loadCompositionConfig(t, path)
	dbPath, _, _, err := storagePaths(result.Config)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(t.Context(), dbPath, sqlite.Options{})
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
			if b.server.checks[i].ID == "workflow_definitions" {
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
		if !fault.HasCode(err, auraagent.ErrorCodePersonaInvalid) {
			t.Fatalf("Build error = %v, want %s", err, auraagent.ErrorCodePersonaInvalid)
		}
	})
}

func hasConfigCode(err error, want auraagent.ErrorCode) bool {
	return fault.HasCode(err, want)
}

func TestQuarantinedRoutineIsHiddenFromTheControllerTools(t *testing.T) {
	controller, _, _ := workflowToolFixture(t, "book_visit")
	registry, err := auraagent.Build([]auraagent.Persona{{
		ID: "desk", Description: "desk", Instructions: "You help.", Audience: auraagent.AudienceTrusted,
		Tools: []string{"workflow_start"}, PrincipalTools: []string{"workflow_start"}, Workflows: []string{"book_visit", "broken_routine"},
	}}, definitionNames(), appconfig.BuiltinToolDefinitions(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workflow.NewController(&workflow.ControllerDeps{Interpreter: controller.Interpreter(), Runs: controller.Store(), Registry: registry}); !hasConfigCode(err, auraagent.ErrorCodePersonaInvalid) {
		t.Fatalf("controller without the quarantine list = %v, want %s", err, auraagent.ErrorCodePersonaInvalid)
	}
	quarantined, err := workflow.NewController(&workflow.ControllerDeps{Interpreter: controller.Interpreter(), Runs: controller.Store(), Registry: registry, Quarantined: []string{"broken_routine"}})
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
