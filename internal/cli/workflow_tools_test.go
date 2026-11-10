package cli

import (
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	toolsbuiltin "github.com/anggasct/aura/internal/modules/builtin"
	"github.com/anggasct/aura/internal/modules/workflow"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/principal"
)

func waitingSpec(id, goal string) *workflow.Spec {
	event := id + ".done"
	return &workflow.Spec{
		ID: id, Goal: goal, Version: 1, Source: workflow.SourceDefined,
		Steps: []workflow.StepSpec{{ID: "hold", Executor: workflow.ExecutorSpec{Kind: workflow.KindWait, Event: &event}, Timeout: time.Minute}},
	}
}

func workflowToolFixture(t *testing.T, allowlist ...string) (*workflowToolController, *workflow.Store, *sql.DB) {
	t.Helper()
	db := openAccessDB(t, writePersonaEvalConfig(t, "x", "web_search"))
	runs := workflow.NewStore(db)
	fake := durable.NewFake()
	t.Cleanup(fake.Close)
	interpreter := workflow.NewInterpreter(runs, fake, &workflow.Options{Logger: slog.New(slog.DiscardHandler)})
	deps := workflow.ValidationDeps{KnownTools: toolsbuiltin.DefinitionNames(), EffectfulTools: toolsbuiltin.EffectfulToolNames()}
	for _, spec := range []*workflow.Spec{waitingSpec("book_visit", "Book a visit"), waitingSpec("refund", "Issue a refund")} {
		if err := interpreter.Load(t.Context(), spec, deps); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := auraagent.Build([]config.AgentDefinition{{
		ID: "lobby", Description: "front desk", Instructions: "You help visitors.", Audience: config.AudiencePublic,
		Tools: []string{"workflow_start"}, PublicTools: []string{"workflow_start"}, Workflows: allowlist,
	}}, toolsbuiltin.DefinitionNames(), nil)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := newWorkflowToolController(interpreter, runs, registry)
	if err != nil {
		t.Fatal(err)
	}
	return controller, runs, db
}

func caller(class principal.Class, id string) *toolsbuiltin.WorkflowCaller {
	return &toolsbuiltin.WorkflowCaller{SessionID: "lobby/discord:" + id + "/dm", PrincipalID: id, PrincipalClass: class, AgentID: "lobby"}
}

func listedIDs(t *testing.T, controller *workflowToolController, who *toolsbuiltin.WorkflowCaller) string {
	t.Helper()
	workflows, err := controller.List(t.Context(), who)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(workflows))
	for _, info := range workflows {
		ids = append(ids, info.ID)
	}
	return strings.Join(ids, ",")
}

func TestOwnerStartsAnyLoadedWorkflowAndReadsItsSteps(t *testing.T) {
	controller, runs, _ := workflowToolFixture(t, "book_visit")
	owner := caller(principal.ClassOwner, principal.OwnerID)
	if got := listedIDs(t, controller, owner); got != "book_visit,refund" {
		t.Fatalf("owner list = %q", got)
	}
	started, err := controller.Start(t.Context(), owner, "refund", "  refund order 42  ")
	if err != nil || started.RunID == "" || started.WorkflowID != "refund" {
		t.Fatalf("Start = %+v, %v", started, err)
	}
	run, err := runs.Run(t.Context(), started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Origin != (workflow.RunOrigin{PrincipalID: principal.OwnerID, PrincipalClass: string(principal.ClassOwner), AgentID: "lobby", SessionID: owner.SessionID}) {
		t.Fatalf("origin = %+v", run.Origin)
	}
	input, err := runs.RunInputFor(t.Context(), started.RunID)
	if err != nil || input.Objective != "refund order 42" || len(input.Permissions) != 0 || len(input.Resources) != 0 {
		t.Fatalf("run input = %+v, %v", input, err)
	}
	status, err := controller.Status(t.Context(), owner, started.RunID)
	if err != nil || status.WorkflowID != "refund" || len(status.Steps) != 1 || status.Steps[0].ID != "hold" {
		t.Fatalf("Status = %+v, %v", status, err)
	}
	if _, err := controller.Start(t.Context(), owner, "missing", ""); err == nil || errors.Is(err, toolsbuiltin.ErrWorkflowNotPermitted) {
		t.Fatalf("owner start of an unloaded workflow = %v", err)
	}
}

func TestNonOwnersStartOnlyAllowlistedWorkflowsAndSeeOnlyTheirRuns(t *testing.T) {
	controller, _, _ := workflowToolFixture(t, "book_visit")
	visitor := caller(principal.ClassPublic, "discord:111")
	other := caller(principal.ClassTrusted, "discord:222")
	owner := caller(principal.ClassOwner, principal.OwnerID)
	if got := listedIDs(t, controller, visitor); got != "book_visit" {
		t.Fatalf("public list = %q", got)
	}
	for _, id := range []string{"refund", "missing"} {
		if _, err := controller.Start(t.Context(), visitor, id, ""); !errors.Is(err, toolsbuiltin.ErrWorkflowNotPermitted) {
			t.Fatalf("public start of %q = %v, want workflow_not_permitted", id, err)
		}
	}
	started, err := controller.Start(t.Context(), visitor, "book_visit", "Saturday 10am")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Status(t.Context(), visitor, started.RunID); err != nil {
		t.Fatalf("own status: %v", err)
	}
	if _, err := controller.Status(t.Context(), owner, started.RunID); err != nil {
		t.Fatalf("owner status of a visitor run: %v", err)
	}
	ownerRun, err := controller.Start(t.Context(), owner, "book_visit", "")
	if err != nil {
		t.Fatal(err)
	}
	for label, probe := range map[string]struct {
		who   *toolsbuiltin.WorkflowCaller
		runID string
	}{
		"another principal's run":   {other, started.RunID},
		"the owner's run":           {visitor, ownerRun.RunID},
		"a run that does not exist": {visitor, "run_nope"},
	} {
		if _, err := controller.Status(t.Context(), probe.who, probe.runID); !errors.Is(err, toolsbuiltin.ErrWorkflowRunNotFound) {
			t.Fatalf("status of %s = %v, want workflow_run_not_found", label, err)
		}
		if _, err := controller.Cancel(t.Context(), probe.who, probe.runID); !errors.Is(err, toolsbuiltin.ErrWorkflowRunNotFound) {
			t.Fatalf("cancel of %s = %v, want workflow_run_not_found", label, err)
		}
	}
	if _, err := controller.Cancel(t.Context(), visitor, started.RunID); err != nil {
		t.Fatalf("own cancel: %v", err)
	}
}

func TestAllowlistNamingAnUnloadedWorkflowFailsStartup(t *testing.T) {
	db := openAccessDB(t, writePersonaEvalConfig(t, "x", "web_search"))
	runs := workflow.NewStore(db)
	interpreter := workflow.NewInterpreter(runs, durable.NewFake(), &workflow.Options{Logger: slog.New(slog.DiscardHandler)})
	registry, err := auraagent.Build([]config.AgentDefinition{{ID: "lobby", Description: "d", Instructions: "x", Workflows: []string{"ghost"}}}, toolsbuiltin.DefinitionNames(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = newWorkflowToolController(interpreter, runs, registry)
	var configErr *config.Error
	if !errors.As(err, &configErr) || configErr.Code != config.ErrorCodePersonaInvalid || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("startup with an unknown allowlisted workflow = %v", err)
	}
}
