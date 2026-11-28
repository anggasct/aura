package app

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/modules/schedule"
	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/principal"
)

const unsafeWorkflowYAML = `id: leaky
version: 1
goal: Read the web, then write a file
source: defined
steps:
  - id: search
    timeout: 1m
    executor: { kind: tool, tool: web_search, args: { query: releases } }
  - id: gate
    timeout: 1m
    depends_on: [search]
    executor: { kind: approval }
  - id: save
    timeout: 1m
    depends_on: [gate]
    executor: { kind: tool, tool: write_file, args: { path: notes.txt, content: x } }
`

const safeWorkflowYAML = `id: tidy
version: 1
goal: Read the web, then read a file
source: defined
steps:
  - id: search
    timeout: 1m
    executor: { kind: tool, tool: web_search, args: { query: releases } }
  - id: read
    timeout: 1m
    depends_on: [search]
    executor: { kind: tool, tool: read_file, args: { path: notes.txt } }
`

func scheduledWorkflowFixture(t *testing.T) *GlobalFlags {
	t.Helper()
	gf := writeWorkflowFixtures(t, "")
	definitions := filepath.Join(filepath.Dir(gf.ConfigPath), "workflows")
	for name, content := range map[string]string{"leaky.yaml": unsafeWorkflowYAML, "tidy.yaml": safeWorkflowYAML} {
		if err := os.WriteFile(filepath.Join(definitions, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return gf
}

func cronAddWorkflow(t *testing.T, gf *GlobalFlags, id string) (string, error) {
	t.Helper()
	return runScheduleCommand(t, gf, "add", "--name", "weekly", "--schedule", "0 3 * * 1", "--channel", "discord", "--destination", "default", "--workflow", id)
}

func TestCronAddRefusesAScheduledWorkflowWithASinkAfterAWebStep(t *testing.T) {
	gf := scheduledWorkflowFixture(t)
	if _, err := cronAddWorkflow(t, gf, "leaky"); err == nil || !strings.Contains(err.Error(), string(workflow.ErrorCodeUnattendedSink)) {
		t.Fatalf("cron add of a workflow that writes after the web = %v, want %s", err, workflow.ErrorCodeUnattendedSink)
	}
	if out, err := cronAddWorkflow(t, gf, "tidy"); err != nil || !strings.Contains(out, "id: ") {
		t.Fatalf("cron add of a sink-free workflow = %q, %v, want it scheduled", out, err)
	}
}

func TestCronConfirmRefusesAScheduledWorkflowWithASinkAfterAWebStep(t *testing.T) {
	gf := scheduledWorkflowFixture(t)
	db := openAccessDB(t, gf.ConfigPath)
	jobs, err := schedule.NewSQLiteStore(db)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for id, target := range map[string]string{"job_0000000000000009": "leaky", "job_000000000000000a": "tidy"} {
		if err := jobs.InsertJob(t.Context(), &schedule.Job{
			ID: id, Name: "legacy", CronExpression: "0 3 * * 1", Timezone: "UTC", WorkflowID: target,
			OriginChannel: "discord", OriginDestination: "default", OverlapPolicy: schedule.OverlapSkip,
			State: schedule.JobActive, Version: 1, CreatedAt: now, UpdatedAt: now,
			Tainted: true, TaintSource: "a webhook", Context: schedule.ContextFull,
		}); err != nil {
			t.Fatalf("InsertJob: %v", err)
		}
	}
	if _, err := runScheduleCommand(t, gf, "confirm", "job_0000000000000009"); err == nil || !strings.Contains(err.Error(), string(workflow.ErrorCodeUnattendedSink)) {
		t.Fatalf("cron confirm of the leaky workflow = %v, want %s", err, workflow.ErrorCodeUnattendedSink)
	}
	if out, err := runScheduleCommand(t, gf, "confirm", "job_000000000000000a"); err != nil || !strings.Contains(out, "confirm with:") {
		t.Fatalf("cron confirm of the sink-free workflow = %q, %v, want a preview with its code", out, err)
	}
}

func TestScheduledFireOfAnUnsafeWorkflowFailsWithTheOwnerNotice(t *testing.T) {
	controller, runs, _ := workflowToolFixture(t)
	interpreter := controller.Interpreter()
	deps := &workflow.ValidationDeps{
		Agents: builtinRegistry(t), KnownTools: definitionNames(), EffectfulTools: effectfulToolNames(), SinkTools: []string{"read_file"},
		CheckToolCall: acceptToolCall, StepAllowed: acceptStepAllowed,
	}
	read, write := "read_file", "write_file"
	leaky := &workflow.Spec{ID: "leaky", Goal: "Read, ask, then write", Version: 1, Source: workflow.SourceDefined, Steps: []workflow.StepSpec{
		{ID: "one", Executor: workflow.ExecutorSpec{Kind: workflow.KindTool, ToolID: &read, ToolArgs: []byte(`{"path":"a"}`)}, Timeout: time.Minute},
		{ID: "gate", DependsOn: []string{"one"}, Executor: workflow.ExecutorSpec{Kind: workflow.KindApproval}, Timeout: time.Minute},
		{ID: "two", DependsOn: []string{"gate"}, Executor: workflow.ExecutorSpec{Kind: workflow.KindTool, ToolID: &write, ToolArgs: []byte(`{"path":"a","content":"b"}`)}, Timeout: time.Minute},
	}}
	if err := interpreter.Install(t.Context(), []*workflow.Spec{leaky}, deps); err != nil {
		t.Fatalf("Install: %v", err)
	}
	workflows := &scheduleWorkflows{}
	workflows.set(interpreter)

	refused, err := workflows.start(t.Context(), &schedule.TurnRequest{PrincipalID: principal.OwnerID, IdempotencyKey: "cron/job-1/one", WorkflowID: "leaky"})
	if err != nil || refused.Succeeded || refused.TurnID != "" || refused.SafeErrorCode != string(workflow.ErrorCodeUnattendedSink) || !strings.Contains(refused.Text, "did not start") {
		t.Fatalf("a fire of a workflow that writes after the web = %+v, %v, want a failed result with the owner notice", refused, err)
	}
	if listed, err := runs.Runs(t.Context(), ""); err != nil || len(listed) != 0 {
		t.Errorf("the refused fire left runs %v, %v", listed, err)
	}
	started, err := workflows.start(t.Context(), &schedule.TurnRequest{PrincipalID: principal.OwnerID, IdempotencyKey: "cron/job-1/two", WorkflowID: "book_visit"})
	if err != nil || !started.Succeeded || started.TurnID == "" {
		t.Fatalf("a fire of a safe workflow = %+v, %v, want it started", started, err)
	}
}

func TestScheduledWorkflowOfATaintedJobRunsTaintedThroughTheFire(t *testing.T) {
	controller, runs, _ := workflowToolFixture(t)
	workflows := &scheduleWorkflows{}
	workflows.set(controller.Interpreter())
	for name, tc := range map[string]struct {
		source  string
		tainted bool
	}{"a clean job": {}, "a tainted job": {source: "a webhook", tainted: true}} {
		t.Run(name, func(t *testing.T) {
			result, err := workflows.start(t.Context(), &schedule.TurnRequest{
				PrincipalID: principal.OwnerID, IdempotencyKey: "cron/" + name, WorkflowID: "book_visit", TaintSource: tc.source,
			})
			if err != nil || !result.Succeeded {
				t.Fatalf("fire = %+v, %v", result, err)
			}
			run, err := runs.Run(t.Context(), result.TurnID)
			if err != nil || run.Origin.Tainted != tc.tainted {
				t.Errorf("run = %+v, %v, want tainted=%t", run.Origin, err, tc.tainted)
			}
		})
	}
}

func TestCronConfirmThroughTheCLIAppliesOnlyWithThePrintedCode(t *testing.T) {
	gf := scheduledWorkflowFixture(t)
	db := openAccessDB(t, gf.ConfigPath)
	jobs, err := schedule.NewSQLiteStore(db)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	const id = "job_00000000000000d1"
	if err := jobs.InsertJob(t.Context(), &schedule.Job{
		ID: id, Name: "legacy", CronExpression: "0 3 * * 1", Timezone: "UTC", Prompt: "summarize", Context: schedule.ContextFull,
		OriginChannel: "discord", OriginDestination: "default", OverlapPolicy: schedule.OverlapSkip,
		State: schedule.JobActive, Version: 1, CreatedAt: now, UpdatedAt: now, Tainted: true, TaintSource: "a webhook",
	}); err != nil {
		t.Fatalf("InsertJob: %v", err)
	}
	preview, err := runScheduleCommand(t, gf, "confirm", id, "--context", "web_only")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	match := cliConfirmLine.FindStringSubmatch(lastLine(preview))
	if match == nil || !strings.Contains(preview, "Context changes to: web_only") || !strings.Contains(preview, "a webhook") {
		t.Fatalf("preview = %q, want the new context, the label and the code line", preview)
	}
	if _, err := runScheduleCommand(t, gf, "confirm", id, "--context", "web_only", "--apply", "0000-0000"); err == nil || !strings.Contains(err.Error(), "operation_confirmation_invalid") {
		t.Fatalf("apply with a wrong code = %v, want operation_confirmation_invalid", err)
	}
	if _, err := runScheduleCommand(t, gf, "confirm", id, "--apply", match[2]); err == nil {
		t.Fatal("a code printed for one request confirmed another")
	}
	if out, err := runScheduleCommand(t, gf, "confirm", id, "--context", "web_only", "--apply", match[2]); err != nil || !strings.Contains(out, "tainted: false") || !strings.Contains(out, "context: web_only") {
		t.Fatalf("apply = %q, %v, want the confirmed job", out, err)
	}
	listed, err := runScheduleCommand(t, gf, "list")
	if err != nil || !strings.Contains(listed, "web_only\tfalse\t") {
		t.Errorf("list = %q, %v, want the job clean and web-only", listed, err)
	}
}

var cliConfirmLine = regexp.MustCompile(`^confirm with: (aura cron confirm .*) --apply (\S+)$`)

func lastLine(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	return lines[len(lines)-1]
}
