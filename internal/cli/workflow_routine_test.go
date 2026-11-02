package cli

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/scheduler"
	toolsbuiltin "github.com/anggasct/aura/internal/tools/builtin"
	"github.com/anggasct/aura/internal/workflow"
)

func succeededGeneratedRun(t *testing.T, controller *workflowToolController, runs *workflow.Store) string {
	t.Helper()
	event := "done"
	generated := workflow.NormalizeProposal(&workflow.Spec{Goal: "Weekly report", Steps: []workflow.StepSpec{
		{ID: "hold", Executor: workflow.ExecutorSpec{Kind: workflow.KindWait, Event: &event}, Timeout: time.Minute},
	}}, "prop_weekly")
	if err := controller.interpreter.LoadAll(t.Context(), []*workflow.Spec{generated}, controller.interpreter.ValidationDeps()); err != nil {
		t.Fatal(err)
	}
	run, err := controller.interpreter.Start(t.Context(), generated.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	finishRun(t, runs, run.ID, workflow.RunSucceeded)
	return run.ID
}

func TestOwnerSavesRoutinesThatTheSchedulerStartsOncePerFire(t *testing.T) {
	controller, runs, _ := workflowToolFixture(t, "book_visit")
	runID := succeededGeneratedRun(t, controller, runs)
	owner := caller(principal.ClassOwner, principal.OwnerID)
	if _, err := controller.SaveRoutine(t.Context(), caller(principal.ClassTrusted, "discord:111"), runID, "Weekly report"); !errors.Is(err, toolsbuiltin.ErrWorkflowNotPermitted) {
		t.Fatalf("trusted save = %v", err)
	}
	if _, err := controller.SaveRoutine(t.Context(), owner, "run_missing", "Weekly report"); !errors.Is(err, toolsbuiltin.ErrWorkflowRunNotFound) {
		t.Fatalf("save of a missing run = %v", err)
	}
	var rejection *toolsbuiltin.WorkflowRejection
	if _, err := controller.SaveRoutine(t.Context(), owner, runID, "bad/name"); !errors.As(err, &rejection) || rejection.Code != string(workflow.ErrorCodeRoutineInvalid) {
		t.Fatalf("save with a bad name = %v", err)
	}
	first, err := controller.SaveRoutine(t.Context(), owner, runID, "Weekly report")
	if err != nil || first != (toolsbuiltin.WorkflowRoutine{ID: "routine_weekly_report", Version: 1}) {
		t.Fatalf("first save = %+v, %v", first, err)
	}
	if second, err := controller.SaveRoutine(t.Context(), owner, runID, "Weekly report"); err != nil || second.Version != 2 {
		t.Fatalf("second save = %+v, %v", second, err)
	}
	listed, err := controller.List(t.Context(), owner)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, info := range listed {
		found = found || (info.ID == "routine_weekly_report" && info.Version == 2)
		if strings.HasPrefix(info.ID, "gen_") {
			t.Fatalf("a proposal definition is listed: %s", info.ID)
		}
	}
	if !found {
		t.Fatalf("routine is not listed for the owner: %+v", listed)
	}
	workflows := &scheduleWorkflows{}
	turns := &engineTurnRunner{workflows: workflows}
	request := &scheduler.TurnRequest{PrincipalID: principal.OwnerID, IdempotencyKey: "cron/job-1/2026-11-09T03:00:00Z", WorkflowID: "routine_weekly_report", WorkflowInput: `{"objective":"last week"}`}
	if _, err := turns.RunTurn(t.Context(), request); err == nil || !strings.Contains(err.Error(), "durable runtime") {
		t.Fatalf("scheduled workflow without a runtime = %v", err)
	}
	workflows.set(controller.interpreter)
	fired, err := turns.RunTurn(t.Context(), request)
	if err != nil || !fired.Succeeded || !strings.Contains(fired.Text, "started workflow routine_weekly_report") {
		t.Fatalf("fire = %+v, %v", fired, err)
	}
	replayed, err := turns.RunTurn(t.Context(), request)
	if err != nil || replayed.TurnID != fired.TurnID {
		t.Fatalf("a replayed fire started %+v, %v instead of reusing %s", replayed, err, fired.TurnID)
	}
	run, err := runs.Run(t.Context(), fired.TurnID)
	if err != nil || run.DefinitionID != "routine_weekly_report" || run.DefinitionVersion != 2 {
		t.Fatalf("scheduled run = %+v, %v", run, err)
	}
	input, err := runs.RunInputFor(t.Context(), run.ID)
	if err != nil || input.Objective != "last week" {
		t.Fatalf("scheduled input = %+v, %v", input, err)
	}
}

func TestScheduleAddNeedsOneKnownTarget(t *testing.T) {
	cfg := writePersonaEvalConfig(t, "x", "web_search")
	for label, tc := range map[string]struct {
		args []string
		want string
	}{
		"neither":          {args: nil, want: "exactly one of --prompt or --workflow"},
		"both":             {args: []string{"--prompt", "p", "--workflow", "w"}, want: "exactly one of --prompt or --workflow"},
		"input alone":      {args: []string{"--prompt", "p", "--input", "{}"}, want: "--input requires --workflow"},
		"unknown workflow": {args: []string{"--workflow", "ghost"}, want: `"ghost" is neither a definition nor a saved routine`},
		"bad input":        {args: []string{"--workflow", "ghost", "--input", `{"permissions":["exec"]}`}, want: "--input must be a JSON object"},
	} {
		args := append([]string{"add", "--name", "n", "--schedule", "0 3 * * 1", "--channel", "discord", "--destination", "default"}, tc.args...)
		_, err := runAccessCommand(t, newScheduleCmd, cfg, args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", label, err, tc.want)
		}
	}
}
