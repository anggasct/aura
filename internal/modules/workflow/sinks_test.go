package workflow

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/principal"
)

func sinkDeps() *ValidationDeps {
	return &ValidationDeps{SinkTools: []string{"web_fetch", "web_search"}, EffectfulTools: []string{"write_file", "email_send"}}
}

func sinkWebAgent(id string, tools ...string) StepSpec {
	return agentStep(id, agentExec("Look it up.", "text", tools...))
}

func after(step StepSpec, ids ...string) StepSpec {
	step.DependsOn = ids
	return step
}

func sinkPlainAgent(id string, dependsOn ...string) StepSpec {
	return agentStep(id, agentExec("Write it up.", "text"), dependsOn...)
}

func sinkToolStep(id, tool string, dependsOn ...string) StepSpec {
	return StepSpec{ID: id, Executor: ExecutorSpec{Kind: KindTool, ToolID: &tool, ToolArgs: json.RawMessage(`{}`)}, DependsOn: dependsOn, Timeout: time.Minute}
}

func sinkKindStep(id string, kind Kind, dependsOn ...string) StepSpec {
	executor := ExecutorSpec{Kind: kind}
	switch kind {
	case KindWait:
		executor.Event = ptr("done")
	case KindWorkflow:
		executor.WorkflowID = ptr("child")
	case KindAgent, KindTool, KindApproval:
	}
	return StepSpec{ID: id, Executor: executor, DependsOn: dependsOn, Timeout: time.Minute}
}

func TestScheduledWorkflowRefusesASinkAfterAWebStep(t *testing.T) {
	onResearch := "steps.research.status == \"succeeded\""
	conditional := sinkToolStep("notify", "write_file")
	conditional.Condition = &onResearch
	cases := map[string][]StepSpec{
		"an effectful tool step":              {sinkWebAgent("research", "web_search"), sinkKindStep("gate", KindApproval, "research"), sinkToolStep("send", "email_send", "gate")},
		"an external sink tool step":          {sinkWebAgent("research", "web_search"), sinkToolStep("fetch", "web_fetch", "research")},
		"a second web agent step":             {sinkWebAgent("research", "web_search"), after(sinkWebAgent("again", "web_fetch"), "research")},
		"a child workflow":                    {sinkWebAgent("research", "web_fetch"), sinkKindStep("child", KindWorkflow, "research")},
		"a step that only reads the status":   {sinkWebAgent("research", "web_search"), sinkPlainAgent("quiet"), conditional},
		"a sink behind a clean step":          {sinkWebAgent("research", "web_search"), sinkPlainAgent("summarize", "research"), sinkToolStep("send", "write_file", "summarize")},
		"a web tool step then a sink":         {sinkToolStep("search", "web_search"), sinkToolStep("send", "write_file", "search")},
		"a sink behind two levels of waiting": {sinkWebAgent("research", "web_fetch"), sinkKindStep("pause", KindWait, "research"), sinkKindStep("gate", KindApproval, "pause"), sinkToolStep("send", "write_file", "gate")},
	}
	for name, steps := range cases {
		t.Run(name, func(t *testing.T) {
			err := CheckUnattendedSinks(agentSpec("job", steps...), sinkDeps())
			if !hasCode(err, ErrorCodeUnattendedSink) {
				t.Errorf("CheckUnattendedSinks = %v, want %s", err, ErrorCodeUnattendedSink)
			}
		})
	}
}

func TestTwoStepJobUsesPrivateContextWithoutSinks(t *testing.T) {
	cases := map[string][]StepSpec{
		"a web step then an agent step without web tools": {sinkWebAgent("research", "web_search", "web_fetch"), sinkPlainAgent("write", "research")},
		"a tool search then an agent step":                {sinkToolStep("search", "web_search"), sinkPlainAgent("write", "search")},
		"an approval and a wait after the web step":       {sinkWebAgent("research", "web_fetch"), sinkKindStep("gate", KindApproval, "research"), sinkKindStep("pause", KindWait, "gate")},
		"a read tool after the web step":                  {sinkWebAgent("research", "web_search"), sinkToolStep("read", "read_file", "research")},
		"an effectful step that does not follow the web":  {sinkWebAgent("research", "web_search"), sinkPlainAgent("write", "research"), sinkKindStep("gate", KindApproval), sinkToolStep("save", "write_file", "gate")},
		"no web step at all":                              {sinkToolStep("read", "read_file"), sinkKindStep("gate", KindApproval, "read"), sinkToolStep("save", "write_file", "gate")},
	}
	for name, steps := range cases {
		t.Run(name, func(t *testing.T) {
			if err := CheckUnattendedSinks(agentSpec("job", steps...), sinkDeps()); err != nil {
				t.Errorf("CheckUnattendedSinks = %v, want the workflow allowed", err)
			}
		})
	}
	t.Run("the check needs a spec and the tool sets", func(t *testing.T) {
		if err := CheckUnattendedSinks(nil, sinkDeps()); !hasCode(err, ErrorCodeInvalidArgument) {
			t.Errorf("nil spec = %v", err)
		}
		if err := CheckUnattendedSinks(agentSpec("job"), nil); !hasCode(err, ErrorCodeInvalidArgument) {
			t.Errorf("nil deps = %v", err)
		}
	})
}

func TestScheduledFireRechecksTheWorkflow(t *testing.T) {
	interpreter, _, disk := driverTestSetup(t, &fakeToolRunner{}, DriverConcurrent)
	deps := testValidationDeps()
	if err := interpreter.Install(t.Context(), []*Spec{twoStepCeilingSpec("later")}, deps); err != nil {
		t.Fatalf("Install: %v", err)
	}
	owner := func(scheduled bool) *ConfiguredStart {
		return &ConfiguredStart{DefinitionID: "later", PrincipalID: principal.OwnerID, Class: principal.ClassOwner, Scheduled: scheduled}
	}
	if _, err := interpreter.StartConfigured(t.Context(), owner(true)); err != nil {
		t.Fatalf("a scheduled start of a workflow with no web step: %v", err)
	}

	changed := testValidationDeps()
	changed.SinkTools = []string{"read_file"}
	if err := interpreter.Install(t.Context(), []*Spec{twoStepCeilingSpec("later")}, changed); err != nil {
		t.Fatalf("Install again: %v", err)
	}
	before := countRuns(t, disk)
	if _, err := interpreter.StartConfigured(t.Context(), owner(true)); !hasCode(err, ErrorCodeUnattendedSink) {
		t.Fatalf("a scheduled start after the tool sets changed = %v, want %s", err, ErrorCodeUnattendedSink)
	}
	if countRuns(t, disk) != before {
		t.Error("a refused scheduled start created a run")
	}
	if _, err := interpreter.StartConfigured(t.Context(), owner(false)); err != nil {
		t.Errorf("an unscheduled start is refused for a rule that only binds scheduled runs: %v", err)
	}
}

func TestScheduledWorkflowOfATaintedJobRunsTainted(t *testing.T) {
	interpreter, _, _ := driverTestSetup(t, &fakeToolRunner{}, DriverConcurrent)
	installAll(t, interpreter, toolSpec("plain"))
	for name, tainted := range map[string]bool{"a clean job": false, "a tainted job": true} {
		t.Run(name, func(t *testing.T) {
			summary, err := interpreter.StartConfigured(t.Context(), &ConfiguredStart{
				DefinitionID: "plain", PrincipalID: principal.OwnerID, Class: principal.ClassOwner, Scheduled: true, Tainted: tainted,
			})
			if err != nil || summary.Origin.Tainted != tainted {
				t.Errorf("run = %+v, %v, want tainted=%t", summary, err, tainted)
			}
		})
	}
}
